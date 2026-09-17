// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLonghaulMain(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Long Haul Main Suite")
}
