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
	"log/slog"
	"regexp"
	"sync"

	"github.com/containerd/cgroups/v3"

	"github.com/NVIDIA/dcgm-exporter/internal/pkg/appconfig"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/collector"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/deviceinfo"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/nvmlprovider"
	"github.com/NVIDIA/dcgm-exporter/internal/pkg/utils"
)

type ociMapper struct {
	Config         *appconfig.Config
	cgroupWarnOnce sync.Once
}

func newOCIMapper(c *appconfig.Config) *ociMapper {
	slog.Info("OCI container metrics mapping enabled")
	return &ociMapper{Config: c}
}

func (p *ociMapper) Name() string {
	return "ociMapper"
}

var parseCgroupFile = cgroups.ParseCgroupFileUnified

// Parsed fresh every call: PIDs are reused after process exit, so caching
// would mislabel short-lived containers.
func (p *ociMapper) getContainerIDForPID(pid uint32) (string, error) {
	cgroupPath := fmt.Sprintf("/proc/%d/cgroup", pid)
	subsystems, unified, err := parseCgroupFile(cgroupPath)
	if err != nil {
		return "", fmt.Errorf("failed to parse cgroup file for PID %d: %w", pid, err)
	}
	return extractContainerIDFromPaths(subsystems, unified), nil
}

func (p *ociMapper) getMappings(deviceInfo deviceinfo.Provider) map[string]map[string]struct{} {
	client := nvmlprovider.Client()
	result := make(map[string]map[string]struct{})

	for i := uint(0); i < deviceInfo.GPUCount(); i++ {
		uuid := deviceInfo.GPU(i).DeviceInfo.UUID

		pidToMem, err := client.GetDeviceProcessMemory(uuid)
		if err != nil {
			slog.Debug("Failed to get GPU process memory", "gpuUUID", uuid, "error", err)
			continue
		}

		for pid := range pidToMem {
			containerID, err := p.getContainerIDForPID(pid)
			if err != nil {
				p.cgroupWarnOnce.Do(func() {
					slog.Warn("Failed to map PID to container; container labels may be incomplete",
						"pid", pid, "error", err)
				})
				continue
			}
			if containerID == "" {
				continue
			}
			if result[uuid] == nil {
				result[uuid] = make(map[string]struct{})
			}
			result[uuid][shortContainerID(containerID)] = struct{}{}
		}
	}
	return result
}

func (p *ociMapper) Process(metrics collector.MetricsByCounter, deviceInfo deviceinfo.Provider) error {
	gpuToContainers := p.getMappings(deviceInfo)
	if len(gpuToContainers) == 0 {
		return nil
	}

	for counter, ms := range metrics {
		var newMetrics []collector.Metric
		for _, metric := range ms {
			containers := gpuToContainers[metric.GPUUUID]
			if len(containers) == 0 {
				newMetrics = append(newMetrics, metric)
				continue
			}
			for shortID := range containers {
				copied, err := utils.DeepCopy(metric)
				if err != nil {
					slog.Error("Failed to deep-copy metric for container labeling", "error", err)
					continue
				}
				if copied.Attributes == nil {
					copied.Attributes = make(map[string]string)
				}
				copied.Attributes[containerAttribute] = shortID
				delete(copied.Labels, containerAttribute)
				newMetrics = append(newMetrics, copied)
			}
		}
		if len(newMetrics) > 0 {
			metrics[counter] = newMetrics
		}
	}
	return nil
}

// Matches OCI container IDs in cgroup paths. A runtime prefix is required, so
// paths like /k8s.io/<id> intentionally do not match. On nesting, innermost wins.
var ociContainerIDRegex = regexp.MustCompile(`(?:docker|cri-containerd|containerd|crio|libpod)[-/]([0-9a-f]{12,})`)

func extractContainerID(path string) string {
	matches := ociContainerIDRegex.FindAllStringSubmatch(path, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

func extractContainerIDFromPaths(subsystems map[string]string, unified string) string {
	for _, p := range subsystems {
		if id := extractContainerID(p); id != "" {
			return id
		}
	}
	return extractContainerID(unified)
}

// 12-char Docker/OCI short-ID convention.
func shortContainerID(id string) string {
	const shortLen = 12
	if len(id) > shortLen {
		return id[:shortLen]
	}
	return id
}
