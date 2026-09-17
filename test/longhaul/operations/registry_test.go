// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operations

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/documentdb/documentdb-operator/test/longhaul/config"
	"github.com/documentdb/documentdb-operator/test/longhaul/journal"
)

var _ = Describe("Registry", func() {
	It("resolves exact stable names in requested order", func() {
		a := &fakeOp{name: "a"}
		b := &fakeOp{name: "b"}
		registry, err := NewRegistry(a, b)
		Expect(err).NotTo(HaveOccurred())

		resolved, err := registry.Resolve([]string{"b", "a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved).To(Equal([]Operation{b, a}))
		Expect(registry.All()).To(Equal([]Operation{a, b}))
	})

	It("rejects unknown requested names", func() {
		registry, err := NewRegistry(&fakeOp{name: "known"})
		Expect(err).NotTo(HaveOccurred())
		_, err = registry.Resolve([]string{"unknown"})
		Expect(err).To(MatchError(ContainSubstring(`unknown name "unknown"`)))
	})

	It("rejects duplicate requested names", func() {
		registry, err := NewRegistry(&fakeOp{name: "known"})
		Expect(err).NotTo(HaveOccurred())
		_, err = registry.Resolve([]string{"known", "known"})
		Expect(err).To(MatchError(ContainSubstring(`duplicate name "known"`)))
	})

	It("rejects duplicate registered operation names", func() {
		_, err := NewRegistry(&fakeOp{name: "same"}, &fakeOp{name: "same"})
		Expect(err).To(MatchError(ContainSubstring(`duplicate name "same"`)))
	})

	It("constructs the default registry with the stable operation names", func() {
		registry, err := NewDefaultRegistry(config.DefaultConfig(), nil, nil, nil, journal.New())
		Expect(err).NotTo(HaveOccurred())

		names := make([]string, 0)
		for _, op := range registry.All() {
			names = append(names, op.Name())
		}
		Expect(names).To(Equal([]string{
			"scale-up",
			"scale-down",
			"upgrade-documentdb",
			"kill-operator-pod",
			"kill-primary-pod",
		}))
	})
})
