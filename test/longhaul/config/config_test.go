// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config", func() {
	Describe("DefaultConfig", func() {
		It("returns safe defaults", func() {
			cfg := DefaultConfig()
			Expect(cfg.MaxDuration).To(Equal(30 * time.Minute))
			Expect(cfg.Namespace).To(Equal("default"))
			Expect(cfg.ClusterName).To(BeEmpty())
			Expect(cfg.OperatorNamespace).To(Equal("documentdb-operator"))
			Expect(cfg.NumWriters).To(Equal(5))
			Expect(cfg.OpCooldown).To(Equal(5 * time.Minute))
			Expect(cfg.RecoveryTimeout).To(Equal(10 * time.Minute))
			Expect(cfg.SteadyStateWait).To(Equal(60 * time.Second))
			Expect(cfg.OperationMode).To(Equal(OperationModeRandom))
			Expect(cfg.OperationSequence).To(BeEmpty())
			Expect(cfg.MinInstances).To(Equal(1))
			Expect(cfg.MaxInstances).To(Equal(3))
			Expect(cfg.RetainPerWriter).To(Equal(int64(DefaultRetainPerWriter)))
		})
	})

	Describe("LoadFromEnv", func() {
		// Clear every LONGHAUL_* env var before each spec so tests do not pick up
		// values from the developer's shell (Copilot review feedback on PR #348).
		BeforeEach(func() {
			for _, k := range []string{
				EnvEnabled, EnvMaxDuration, EnvNamespace, EnvClusterName,
				EnvOperatorNamespace,
				EnvDocumentDBURI, EnvNumWriters,
				EnvOpCooldown, EnvRecoveryTimeout, EnvSteadyStateWait,
				EnvOperationMode, EnvOperationSeq,
				EnvMinInstances, EnvMaxInstances, EnvReportInterval,
				EnvBackupEnabled, EnvBackupSchedule, EnvBackupRetentionDays,
				EnvBackupVerifyInterval,
				EnvResetData, EnvRetainPerWriter,
			} {
				GinkgoT().Setenv(k, "")
			}
		})

		It("uses defaults when no env vars set", func() {
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MaxDuration).To(Equal(30 * time.Minute))
			Expect(cfg.BackupVerifyInterval).To(Equal(5 * time.Minute))
		})

		It("parses MaxDuration from env", func() {
			GinkgoT().Setenv(EnvMaxDuration, "1h")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MaxDuration).To(Equal(1 * time.Hour))
		})

		It("parses zero MaxDuration for infinite runs", func() {
			GinkgoT().Setenv(EnvMaxDuration, "0s")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MaxDuration).To(Equal(time.Duration(0)))
		})

		It("parses Namespace and ClusterName from env", func() {
			GinkgoT().Setenv(EnvNamespace, "test-ns")
			GinkgoT().Setenv(EnvClusterName, "my-cluster")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Namespace).To(Equal("test-ns"))
			Expect(cfg.ClusterName).To(Equal("my-cluster"))
		})

		It("parses OperatorNamespace from env", func() {
			GinkgoT().Setenv(EnvOperatorNamespace, "custom-operator-ns")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.OperatorNamespace).To(Equal("custom-operator-ns"))
		})

		It("returns error for invalid MaxDuration", func() {
			GinkgoT().Setenv(EnvMaxDuration, "not-a-duration")
			_, err := LoadFromEnv()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(EnvMaxDuration))
		})

		It("parses NumWriters from env", func() {
			GinkgoT().Setenv(EnvNumWriters, "10")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.NumWriters).To(Equal(10))
		})

		It("returns error for invalid NumWriters", func() {
			GinkgoT().Setenv(EnvNumWriters, "abc")
			_, err := LoadFromEnv()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(EnvNumWriters))
		})

		It("parses OpCooldown from env", func() {
			GinkgoT().Setenv(EnvOpCooldown, "10m")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.OpCooldown).To(Equal(10 * time.Minute))
		})

		It("parses DocumentDBURI from env", func() {
			GinkgoT().Setenv(EnvDocumentDBURI, "mongodb://localhost:27017")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.DocumentDBURI).To(Equal("mongodb://localhost:27017"))
		})

		It("returns error for invalid RetainPerWriter", func() {
			GinkgoT().Setenv(EnvRetainPerWriter, "not-a-number")
			_, err := LoadFromEnv()
			Expect(err).To(MatchError(ContainSubstring(EnvRetainPerWriter)))
		})

		It("normalizes operation mode and trims sequence names", func() {
			GinkgoT().Setenv(EnvOperationMode, " Sequence ")
			GinkgoT().Setenv(EnvOperationSeq, " kill-operator-pod,  kill-primary-pod ")
			cfg, err := LoadFromEnv()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.OperationMode).To(Equal(OperationModeSequence))
			Expect(cfg.OperationSequence).To(Equal([]string{"kill-operator-pod", "kill-primary-pod"}))
		})

		It("rejects empty names in a non-empty sequence", func() {
			GinkgoT().Setenv(EnvOperationSeq, "scale-up, ,scale-down")
			_, err := LoadFromEnv()
			Expect(err).To(MatchError(ContainSubstring("operation names must not be empty")))
		})
	})

	Describe("Validate", func() {
		It("passes for valid config", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test-cluster"
			Expect(cfg.Validate()).To(Succeed())
		})

		It("fails when Namespace is empty", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.Namespace = ""
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("namespace")))
		})

		It("fails when ClusterName is empty", func() {
			cfg := DefaultConfig()
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("cluster name")))
		})

		It("fails when OperatorNamespace is empty", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.OperatorNamespace = ""
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("operator namespace")))
		})

		It("fails when MaxDuration is negative", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.MaxDuration = -1 * time.Second
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("max duration must not be negative")))
		})

		It("fails when NumWriters is zero", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.NumWriters = 0
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("num writers")))
		})

		It("fails when RecoveryTimeout is zero", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.RecoveryTimeout = 0
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("recovery timeout")))
		})

		It("fails for an unknown operation mode", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.OperationMode = "roulette"
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("operation mode must be one of")))
		})

		It("requires a non-empty sequence in sequence mode", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.OperationMode = OperationModeSequence
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("operation sequence must not be empty")))
		})

		It("rejects duplicate sequence names", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.OperationMode = OperationModeSequence
			cfg.OperationSequence = []string{"scale-up", "scale-up"}
			Expect(cfg.Validate()).To(MatchError(ContainSubstring(`duplicate name "scale-up"`)))
		})

		DescribeTable("rejects a sequence outside sequence mode",
			func(mode OperationMode) {
				cfg := DefaultConfig()
				cfg.ClusterName = "test"
				cfg.OperationMode = mode
				cfg.OperationSequence = []string{"scale-up"}
				Expect(cfg.Validate()).To(MatchError(ContainSubstring("operation sequence must be empty")))
			},
			Entry("random", OperationModeRandom),
			Entry("disabled", OperationModeDisabled),
		)

		It("accepts a valid sequence configuration", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.OperationMode = OperationModeSequence
			cfg.OperationSequence = []string{"kill-operator-pod", "kill-primary-pod"}
			Expect(cfg.Validate()).To(Succeed())
		})

		It("fails when MaxInstances < MinInstances", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.MinInstances = 3
			cfg.MaxInstances = 2
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("max instances")))
		})

		It("fails when MaxInstances exceeds CRD upper bound (3)", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.MinInstances = 1
			cfg.MaxInstances = 4
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("must not exceed 3")))
		})

		It("fails when backups enabled but schedule is empty", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.BackupSchedule = ""
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("backup schedule")))
		})

		It("fails when backup retention days is below 1", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.BackupRetentionDays = 0
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("backup retention days")))
		})

		It("fails when backup verify interval is not positive", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.BackupVerifyInterval = 0
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("backup verify interval")))
		})

		It("skips backup validation when backups disabled", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.BackupEnabled = false
			cfg.BackupSchedule = ""
			cfg.BackupRetentionDays = 0
			Expect(cfg.Validate()).To(Succeed())
		})

		It("fails when RetainPerWriter is negative", func() {
			cfg := DefaultConfig()
			cfg.ClusterName = "test"
			cfg.RetainPerWriter = -1
			Expect(cfg.Validate()).To(MatchError(ContainSubstring("retain per writer")))
		})
	})

	Describe("IsEnabled", func() {
		It("returns false when env not set", func() {
			GinkgoT().Setenv(EnvEnabled, "")
			Expect(IsEnabled()).To(BeFalse())
		})

		It("returns true for 'true'", func() {
			GinkgoT().Setenv(EnvEnabled, "true")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true for '1'", func() {
			GinkgoT().Setenv(EnvEnabled, "1")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true for 'yes'", func() {
			GinkgoT().Setenv(EnvEnabled, "yes")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true case-insensitively", func() {
			GinkgoT().Setenv(EnvEnabled, "TRUE")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true for mixed case 'True'", func() {
			GinkgoT().Setenv(EnvEnabled, "True")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true for mixed case 'YES'", func() {
			GinkgoT().Setenv(EnvEnabled, "YES")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true with surrounding whitespace", func() {
			GinkgoT().Setenv(EnvEnabled, " true ")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns true for ' yes ' with whitespace", func() {
			GinkgoT().Setenv(EnvEnabled, " yes ")
			Expect(IsEnabled()).To(BeTrue())
		})

		It("returns false for whitespace-only", func() {
			GinkgoT().Setenv(EnvEnabled, "   ")
			Expect(IsEnabled()).To(BeFalse())
		})

		It("returns false for 'false'", func() {
			GinkgoT().Setenv(EnvEnabled, "false")
			Expect(IsEnabled()).To(BeFalse())
		})

		It("returns false for '0'", func() {
			GinkgoT().Setenv(EnvEnabled, "0")
			Expect(IsEnabled()).To(BeFalse())
		})

		It("returns false for 'no'", func() {
			GinkgoT().Setenv(EnvEnabled, "no")
			Expect(IsEnabled()).To(BeFalse())
		})
	})
})
