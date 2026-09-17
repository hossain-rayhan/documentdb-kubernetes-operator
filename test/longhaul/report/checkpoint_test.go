// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package report

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/operations"
)

var _ = Describe("CheckpointReporter", func() {
	It("emit() is safe with a nil clientset (logs to stdout, does not panic)", func() {
		r := NewCheckpointReporter(nil, "ns", time.Second, func(bool) Summary {
			return Summary{Result: ResultPass, Duration: time.Minute}
		})
		Expect(func() { r.emit(context.Background(), false) }).NotTo(Panic())
	})

	It("creates the ConfigMap on first emit and labels it identifiably", func() {
		cs := fake.NewSimpleClientset()
		r := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			return Summary{Result: ResultPass, Duration: 2 * time.Hour, OpsExecuted: 5}
		})

		r.emit(context.Background(), false)

		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).To(HaveKey("latest-report"))
		Expect(cm.Data).To(HaveKey("last-updated"))
		Expect(cm.Data).To(HaveKey("result"))
		Expect(cm.Data).To(HaveKeyWithValue("operation-status", ""))
		Expect(cm.Data).To(HaveKeyWithValue("operation-results", "[]"))
		// PASS at intermediate checkpoint is persisted as RUNNING so consumers
		// can distinguish in-flight from final state.
		Expect(cm.Data["result"]).To(Equal("RUNNING"))
		Expect(cm.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "longhaul-test"))
	})

	It("persists FAIL results as FAIL", func() {
		cs := fake.NewSimpleClientset()
		r := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			return Summary{Result: ResultFail, FailReason: "data loss"}
		})

		r.emit(context.Background(), false)

		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data["result"]).To(Equal("FAIL"))
	})

	It("latches a failure incident so a restart cannot erase the FAIL verdict", func() {
		cs := fake.NewSimpleClientset()

		// Process A observes a failure, writes FAIL, and exits.
		procA := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			return Summary{Result: ResultFail, FailReason: "data loss: 3 gaps detected"}
		})
		procA.emit(context.Background(), false)

		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data["result"]).To(Equal("FAIL"))
		Expect(cm.Data["incident-latched"]).To(Equal("true"))
		Expect(cm.Data["incident-count"]).To(Equal("1"))
		Expect(cm.Data["first-failure-reason"]).To(Equal("data loss: 3 gaps detected"))
		Expect(cm.Data["first-failure-time"]).NotTo(BeEmpty())

		// The Deployment restarts. Process B starts with fresh in-memory state
		// (healthy) and writes an intermediate RUNNING checkpoint against the
		// same ConfigMap. The transient result flips to RUNNING, but the durable
		// incident state must be preserved so a monitor still sees the failure.
		procB := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			return Summary{Result: ResultPass, Duration: time.Minute}
		})
		procB.emit(context.Background(), false)

		cm, err = cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data["result"]).To(Equal("RUNNING"), "fresh process resets the transient verdict")
		Expect(cm.Data["incident-latched"]).To(Equal("true"), "incident must survive the restart")
		Expect(cm.Data["incident-count"]).To(Equal("1"), "count is monotonic across restarts")
		Expect(cm.Data["first-failure-reason"]).To(Equal("data loss: 3 gaps detected"))
	})

	It("Updates the existing ConfigMap on subsequent emits", func() {
		cs := fake.NewSimpleClientset()

		calls := 0
		r := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			calls++
			return Summary{Result: ResultPass, Duration: time.Duration(calls) * time.Hour, OpsExecuted: calls * 10}
		})

		r.emit(context.Background(), false)
		cm1, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		report1 := cm1.Data["latest-report"]

		// Fake clientset doesn't bump ResourceVersion automatically, so assert
		// on content change instead.
		r.emit(context.Background(), false)
		cm2, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm2.Data["latest-report"]).NotTo(Equal(report1))
		Expect(calls).To(Equal(2))
	})

	It("persists ordered sequence results as bounded JSON", func() {
		cs := fake.NewSimpleClientset()
		results := []operations.OperationResult{
			{Name: "kill-operator-pod", Status: operations.OperationPassed},
			{Name: "kill-primary-pod", Status: operations.OperationPassed},
		}
		r := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			return Summary{
				Result: ResultPass,
				OperationRun: operations.RunSnapshot{
					Mode:    config.OperationModeSequence,
					Status:  operations.RunStatusComplete,
					Results: results,
				},
			}
		})

		r.emit(context.Background(), true)
		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data["operation-status"]).To(Equal("COMPLETE"))

		var persisted []operations.OperationResult
		Expect(json.Unmarshal([]byte(cm.Data["operation-results"]), &persisted)).To(Succeed())
		Expect(persisted).To(Equal(results))
		Expect(cm.Data).NotTo(HaveKey("operation-aggregates"))
	})

	It("overwrites mode-specific fields instead of retaining stale aggregates", func() {
		cs := fake.NewSimpleClientset()
		random := true
		r := NewCheckpointReporter(cs, "ns", time.Second, func(bool) Summary {
			if random {
				return Summary{
					Result: ResultPass,
					OperationRun: operations.RunSnapshot{
						Mode:       config.OperationModeRandom,
						Status:     operations.RunStatusRunning,
						Aggregates: []operations.OperationAggregate{{Name: "scale-up", Passed: 3}},
					},
				}
			}
			return Summary{
				Result: ResultPass,
				OperationRun: operations.RunSnapshot{
					Mode:    config.OperationModeSequence,
					Status:  operations.RunStatusComplete,
					Results: []operations.OperationResult{{Name: "scale-up", Status: operations.OperationPassed}},
				},
			}
		})

		r.emit(context.Background(), false)
		random = false
		r.emit(context.Background(), true)

		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).NotTo(HaveKey("operation-aggregates"))
		Expect(cm.Data["operation-results"]).To(MatchJSON(`[{"name":"scale-up","status":"PASSED"}]`))
	})

	It("emits the final report exactly once and rejects later checkpoints", func() {
		cs := fake.NewSimpleClientset()
		calls := 0
		r := NewCheckpointReporter(cs, "ns", time.Second, func(final bool) Summary {
			calls++
			Expect(final).To(BeTrue())
			return Summary{Result: ResultPass, Duration: time.Duration(calls) * time.Minute}
		})

		first := r.EmitFinal()
		second := r.EmitFinal()
		r.emit(context.Background(), false)

		Expect(calls).To(Equal(1))
		Expect(second).To(Equal(first))
		cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data["result"]).To(Equal("PASS"))
		Expect(cm.Data["latest-report"]).To(ContainSubstring("**Duration:** 1m0s"))
	})
})
