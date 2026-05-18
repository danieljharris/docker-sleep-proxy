package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ProxyPort         string
	TargetService     string
	TargetServices    []string
	TargetPort        string
	SleepTimeout      time.Duration
	CPUUsageThreshold float64
	CheckInterval     time.Duration
	EndpointPrefix    string
	AllowListMode     bool
	PauseContainers   bool
	DockerHost        string
	StartupBehavior   string // "timeout" (default) or "off"
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

func parseTargetServices(value string) []string {
	var services []string
	for _, entry := range strings.Split(value, ",") {
		service := strings.TrimSpace(entry)
		if service != "" {
			services = append(services, service)
		}
	}
	return services
}

func LoadConfig() Config {
	targetService := os.Getenv("TARGET_SERVICE")
	if targetService == "" {
		panic("TARGET_SERVICE environment variable is required")
	}
	targetServices := parseTargetServices(targetService)
	if len(targetServices) == 0 {
		panic("TARGET_SERVICE must contain at least one service name")
	}

	targetPort := os.Getenv("TARGET_PORT")
	if targetPort == "" {
		panic("TARGET_PORT environment variable is required")
	}

	sleepTimeoutSec := getEnvInt("SLEEP_TIMEOUT", 86400) // 24 hours default
	checkIntervalSec := getEnvInt("CHECK_INTERVAL", 5)   // 5 seconds default

	startupBehavior := getEnv("STARTUP_BEHAVIOR", "timeout")
	if startupBehavior != "timeout" && startupBehavior != "off" {
		panic("STARTUP_BEHAVIOR must be either 'timeout' or 'off'")
	}

	return Config{
		ProxyPort:         getEnv("PROXY_PORT", "8000"),
		TargetService:     targetServices[0],
		TargetServices:    targetServices,
		TargetPort:        targetPort,
		SleepTimeout:      time.Duration(sleepTimeoutSec) * time.Second,
		CPUUsageThreshold: getEnvFloat("CPU_USAGE_THRESHOLD", 0),
		CheckInterval:     time.Duration(checkIntervalSec) * time.Second,
		EndpointPrefix:    getEnv("ENDPOINT_PREFIX", "sleep-proxy"),
		AllowListMode:     getEnvBool("ALLOW_LIST_MODE", false),
		PauseContainers:   getEnvBool("PAUSE_CONTAINERS", false),
		DockerHost:        getEnv("DOCKER_HOST", ""),
		StartupBehavior:   startupBehavior,
	}
}
