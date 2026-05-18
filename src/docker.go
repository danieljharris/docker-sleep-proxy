package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

func (sp *SleepProxy) getProjectContainers(ctx context.Context) ([]types.Container, error) {
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", fmt.Sprintf("com.docker.compose.project=%s", sp.projectName))

	containers, err := sp.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return nil, err
	}

	// Filter out the proxy itself and apply allowlist/denylist logic
	var projectContainers []types.Container
	for _, c := range containers {
		// Skip if it's the proxy container (check full or short ID)
		if c.ID == sp.containerID || (len(sp.containerID) >= 12 && c.ID[:12] == sp.containerID[:12]) {
			continue
		}

		serviceName := c.Labels["com.docker.compose.service"]
		if serviceName == "" || !contains(sp.config.TargetServices, serviceName) {
			continue
		}

		labelValue := c.Labels["sleep-proxy.enable"]

		if sp.config.AllowListMode {
			// Allowlist mode: only include containers explicitly set to "true"
			if labelValue == "true" {
				projectContainers = append(projectContainers, c)
				log.Printf("Including container %s (allowlist mode, sleep-proxy.enable=%s)", c.Names[0], labelValue)
			}
		} else {
			// Denylist mode (default): include everything except containers set to "false"
			if labelValue != "false" {
				projectContainers = append(projectContainers, c)
			} else {
				log.Printf("Excluding container %s (denylist mode, sleep-proxy.enable=%s)", c.Names[0], labelValue)
			}
		}
	}

	return projectContainers, nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (sp *SleepProxy) startContainers(ctx context.Context) error {
	containers, err := sp.getProjectContainers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	log.Printf("Starting %d containers in project '%s'", len(containers), sp.projectName)

	for _, c := range containers {
		if c.State != "running" {
			containerName := c.Names[0]
			if len(containerName) > 0 && containerName[0] == '/' {
				containerName = containerName[1:]
			}
			if c.State == "paused" {
				log.Printf("Unpausing container: %s", containerName)
				if err := sp.dockerClient.ContainerUnpause(ctx, c.ID); err != nil {
					log.Printf("Failed to unpause container %s: %v", containerName, err)
				} else {
					log.Printf("Successfully unpaused: %s", containerName)
				}
				continue
			}
			log.Printf("Starting container: %s (state: %s)", containerName, c.State)
			if err := sp.dockerClient.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
				log.Printf("Failed to start container %s: %v", containerName, err)
			} else {
				log.Printf("Successfully started: %s", containerName)
			}
		}
	}

	return nil
}

func (sp *SleepProxy) getContainerCPUPercent(ctx context.Context, containerID string) (float64, error) {
	statsResp, err := sp.dockerClient.ContainerStats(ctx, containerID, false)
	if err != nil {
		return 0, err
	}
	defer statsResp.Body.Close()

	var stats types.StatsJSON
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		return 0, err
	}

	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || systemDelta <= 0 {
		return 0, nil
	}

	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	return (cpuDelta / systemDelta) * onlineCPUs * 100.0, nil
}

func (sp *SleepProxy) getContainerNetworkBytes(ctx context.Context, containerID string) (uint64, error) {
	statsResp, err := sp.dockerClient.ContainerStats(ctx, containerID, false)
	if err != nil {
		return 0, err
	}
	defer statsResp.Body.Close()

	var stats types.StatsJSON
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		return 0, err
	}

	var total uint64
	for _, network := range stats.Networks {
		total += network.RxBytes + network.TxBytes
	}

	return total, nil
}

func (sp *SleepProxy) hasNetworkActivity(ctx context.Context, containers []types.Container) (bool, error) {
	managedIDs := make(map[string]struct{}, len(containers))

	for _, c := range containers {
		managedIDs[c.ID] = struct{}{}
		if c.State != "running" {
			continue
		}

		totalBytes, err := sp.getContainerNetworkBytes(ctx, c.ID)
		if err != nil {
			return false, fmt.Errorf("failed to get network stats for %s: %w", c.Names[0], err)
		}

		previousBytes, hasPrevious := sp.networkBytes[c.ID]
		sp.networkBytes[c.ID] = totalBytes
		if hasPrevious && totalBytes > previousBytes {
			log.Printf("Container %s network activity detected (%d -> %d bytes)", c.Names[0], previousBytes, totalBytes)
			return true, nil
		}
	}

	for containerID := range sp.networkBytes {
		if _, exists := managedIDs[containerID]; !exists {
			delete(sp.networkBytes, containerID)
		}
	}

	return false, nil
}

func (sp *SleepProxy) hasCPUActivityAboveThreshold(ctx context.Context, containers []types.Container) (bool, error) {
	for _, c := range containers {
		if c.State != "running" {
			continue
		}

		cpuPercent, err := sp.getContainerCPUPercent(ctx, c.ID)
		if err != nil {
			return false, fmt.Errorf("failed to get stats for %s: %w", c.Names[0], err)
		}

		if cpuPercent > sp.config.CPUUsageThreshold {
			log.Printf("Container %s CPU usage %.2f%% exceeds threshold %.2f%%", c.Names[0], cpuPercent, sp.config.CPUUsageThreshold)
			return true, nil
		}
	}

	return false, nil
}

func (sp *SleepProxy) stopContainers(ctx context.Context) error {
	containers, err := sp.getProjectContainers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	if sp.config.PauseContainers {
		log.Printf("Pausing %d containers in project '%s'", len(containers), sp.projectName)
	} else {
		log.Printf("Stopping %d containers in project '%s'", len(containers), sp.projectName)
	}

	timeout := 10
	for _, c := range containers {
		if c.State == "running" {
			containerName := c.Names[0]
			if len(containerName) > 0 && containerName[0] == '/' {
				containerName = containerName[1:]
			}
			if sp.config.PauseContainers {
				log.Printf("Pausing container: %s", containerName)
				if err := sp.dockerClient.ContainerPause(ctx, c.ID); err != nil {
					log.Printf("Failed to pause container %s: %v", containerName, err)
				} else {
					log.Printf("Successfully paused: %s", containerName)
				}
			} else {
				log.Printf("Stopping container: %s", containerName)
				if err := sp.dockerClient.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil {
					log.Printf("Failed to stop container %s: %v", containerName, err)
				} else {
					log.Printf("Successfully stopped: %s", containerName)
				}
			}
		}
	}

	sp.setContainersUp(false)
	return nil
}

func (sp *SleepProxy) monitorActivity(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	log.Printf("Activity monitor started (checking every 10 seconds)")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check actual container state
			containers, err := sp.getProjectContainers(ctx)
			if err != nil {
				log.Printf("Failed to get project containers: %v", err)
				continue
			}

			// Determine if all containers are running
			allRunning := len(containers) > 0
			for _, c := range containers {
				if c.State != "running" {
					allRunning = false
					break
				}
			}

			// Update containersUp state if it's incorrect
			if allRunning && !sp.areContainersUp() {
				log.Printf("Detected containers are now running")
				sp.setContainersUp(true)
			} else if !allRunning && sp.areContainersUp() {
				log.Printf("Detected containers are no longer running")
				sp.setContainersUp(false)
			}

			hasNetworkActivity, err := sp.hasNetworkActivity(ctx, containers)
			if err != nil {
				log.Printf("Failed to evaluate container network activity: %v", err)
				continue
			}
			if hasNetworkActivity {
				sp.updateActivity()
				if !allRunning {
					log.Printf("Network activity detected in target group, starting all target containers")
					if err := sp.startContainers(ctx); err != nil {
						log.Printf("Failed to wake target containers: %v", err)
					} else {
						sp.setContainersUp(true)
					}
				}
				continue
			}

			// Check for timeout only if containers are running
			if sp.areContainersUp() {
				timeSinceActivity := time.Since(sp.getLastActivity())
				if timeSinceActivity > sp.config.SleepTimeout {
					if sp.config.CPUUsageThreshold > 0 {
						hasUsage, err := sp.hasCPUActivityAboveThreshold(ctx, containers)
						if err != nil {
							log.Printf("Failed to evaluate container CPU usage, skipping sleep cycle: %v", err)
							continue
						}
						if hasUsage {
							continue
						}
					}

					log.Printf("No activity for %v (threshold: %v), putting containers to sleep",
						timeSinceActivity.Round(time.Second), sp.config.SleepTimeout)
					if err := sp.stopContainers(ctx); err != nil {
						log.Printf("Failed to put containers to sleep: %v", err)
					} else {
						sp.setContainersUp(false)
						if sp.config.PauseContainers {
							log.Printf("Containers paused successfully")
						} else {
							log.Printf("Containers stopped successfully")
						}
					}
				}
			}
		}
	}
}
