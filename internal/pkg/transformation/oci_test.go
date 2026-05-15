/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package transformation

import (
	"fmt"
	"testing"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockdeviceinfo "github.com/NVIDIA/dcgm-exporter/internal/mocks/pkg/deviceinfo"
	mocknvmlprovider "github.com/NVIDIA/dcgm-exporter/internal/mocks/pkg/nvmlprovider"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/appconfig"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/collector"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/counters"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/deviceinfo"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/nvmlprovider"
)

const (
	cidA = "deadbeefcafebabedeadbeefcafebabedeadbeefcafebabedeadbeefcafebabe"
	cidB = "feedface12345678feedface12345678feedface12345678feedface12345678"
)

func TestExtractContainerID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"docker cgroupfs", "/docker/" + cidA, cidA},
		{"docker systemd slice", "/system.slice/docker-" + cidA + ".scope", cidA},
		{"cri-containerd systemd", "/system.slice/cri-containerd-" + cidA + ".scope", cidA},
		{"containerd systemd", "/system.slice/containerd-" + cidA + ".scope", cidA},
		{"crio systemd", "/system.slice/crio-" + cidA + ".scope", cidA},
		{"podman libpod", "/machine.slice/libpod-" + cidA + ".scope", cidA},
		{"nested innermost wins", "/docker/" + cidA + "/docker/" + cidB, cidB},
		{"containerd k8s.io namespace - no runtime prefix", "/k8s.io/" + cidA, ""},
		{"k8s pod path - not OCI runtime prefix", "/kubepods/besteffort/poda9c80282-3f6b-4d5b-84d5-a137a6668011/abc123", ""},
		{"host process", "/user.slice/user-1000.slice/session-1.scope", ""},
		{"empty path", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, extractContainerID(tc.path))
		})
	}
}

func TestExtractContainerIDFromPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		subsystems map[string]string
		unified    string
		expected   string
	}{
		{
			name:       "found in subsystems",
			subsystems: map[string]string{"memory": "/docker/" + cidA, "cpu": "/docker/" + cidA},
			expected:   cidA,
		},
		{
			name:     "found in unified only",
			unified:  "/system.slice/docker-" + cidA + ".scope",
			expected: cidA,
		},
		{
			name:       "not found anywhere",
			subsystems: map[string]string{"memory": "/user.slice/user-1000.slice"},
			unified:    "/system.slice/dcgm-exporter.service",
			expected:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, extractContainerIDFromPaths(tc.subsystems, tc.unified))
		})
	}
}

func TestOCIProcess(t *testing.T) {
	const (
		gpuUsedA = "gpu-uuid-a"
		gpuUsedB = "gpu-uuid-b"
		gpuIdleC = "gpu-uuid-c"

		pidA1     = uint32(1001)
		pidA2     = uint32(1002) // same container as pidA1 → de-duplicated by set
		pidB      = uint32(2001)
		pidOrphan = uint32(3001) // host process, no container
	)
	shortA := cidA[:12]
	shortB := cidB[:12]

	counter := counters.Counter{
		FieldID:   155,
		FieldName: "DCGM_FI_DEV_POWER_USAGE",
		PromType:  "gauge",
	}

	ctrl := gomock.NewController(t)
	mockNVML := mocknvmlprovider.NewMockNVML(ctrl)
	mockNVML.EXPECT().GetDeviceProcessMemory(gpuUsedA).
		Return(map[uint32]uint64{pidA1: 100, pidA2: 200}, nil).AnyTimes()
	mockNVML.EXPECT().GetDeviceProcessMemory(gpuUsedB).
		Return(map[uint32]uint64{pidB: 300, pidOrphan: 400}, nil).AnyTimes()
	mockNVML.EXPECT().GetDeviceProcessMemory(gpuIdleC).
		Return(map[uint32]uint64{}, nil).AnyTimes()
	nvmlprovider.SetClient(mockNVML)

	mockDevInfo := mockdeviceinfo.NewMockProvider(ctrl)
	mockDevInfo.EXPECT().GPUCount().Return(uint(3)).AnyTimes()
	mockDevInfo.EXPECT().GPU(uint(0)).
		Return(deviceinfo.GPUInfo{DeviceInfo: dcgm.Device{UUID: gpuUsedA}}).AnyTimes()
	mockDevInfo.EXPECT().GPU(uint(1)).
		Return(deviceinfo.GPUInfo{DeviceInfo: dcgm.Device{UUID: gpuUsedB}}).AnyTimes()
	mockDevInfo.EXPECT().GPU(uint(2)).
		Return(deviceinfo.GPUInfo{DeviceInfo: dcgm.Device{UUID: gpuIdleC}}).AnyTimes()

	pidToCgroup := map[uint32]string{
		pidA1: "/system.slice/docker-" + cidA + ".scope",
		pidA2: "/system.slice/docker-" + cidA + ".scope",
		pidB:  "/system.slice/cri-containerd-" + cidB + ".scope",
		// pidOrphan deliberately omitted → host process
	}
	orig := parseCgroupFile
	t.Cleanup(func() { parseCgroupFile = orig })
	parseCgroupFile = func(path string) (map[string]string, string, error) {
		var pid uint32
		if _, err := fmt.Sscanf(path, "/proc/%d/cgroup", &pid); err != nil {
			return nil, "", err
		}
		cg, ok := pidToCgroup[pid]
		if !ok {
			return map[string]string{}, "/user.slice/user-1000.slice", nil
		}
		return map[string]string{"memory": cg}, "", nil
	}

	mapper := &ociMapper{Config: &appconfig.Config{OCI: true}}
	metrics := collector.MetricsByCounter{
		counter: {
			{GPU: "0", GPUUUID: gpuUsedA, Counter: counter, Value: "42",
				Labels: map[string]string{containerAttribute: "should-be-cleared"}},
			{GPU: "1", GPUUUID: gpuUsedB, Counter: counter, Value: "84",
				Labels: map[string]string{}, Attributes: map[string]string{}},
			{GPU: "2", GPUUUID: gpuIdleC, Counter: counter, Value: "21",
				Labels: map[string]string{}, Attributes: map[string]string{}},
		},
	}

	require.NoError(t, mapper.Process(metrics, mockDevInfo))

	result := metrics[counter]
	require.Len(t, result, 3, "expected 1 labeled metric per used GPU and 1 preserved idle metric")

	got := map[string]collector.Metric{}
	for _, m := range result {
		got[m.GPUUUID] = m
	}

	// GPU A: pidA1 and pidA2 share a container, fan-out yields one metric with shortA.
	a, ok := got[gpuUsedA]
	require.True(t, ok)
	assert.Equal(t, shortA, a.Attributes[containerAttribute])
	assert.NotContains(t, a.Labels, containerAttribute,
		"Labels[container] must be cleared so it does not collide with Attributes[container]")
	assert.Equal(t, "42", a.Value)

	// GPU B: only pidB maps to a container; pidOrphan is silently skipped.
	b, ok := got[gpuUsedB]
	require.True(t, ok)
	assert.Equal(t, shortB, b.Attributes[containerAttribute])
	assert.Equal(t, "84", b.Value)

	// GPU C: no GPU processes → original device-level metric preserved unmodified.
	c, ok := got[gpuIdleC]
	require.True(t, ok)
	assert.NotContains(t, c.Attributes, containerAttribute)
	assert.Equal(t, "21", c.Value)
}
