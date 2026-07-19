package main

// performance.go provides a bounded, persistent operational history for the
// administrator dashboard. It intentionally uses only the Go standard library
// and Linux /proc files so the APRS service remains a single small binary.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	performanceHistoryFile = "performance_history.jsonl"
	performanceRetention   = 7 * 24 * time.Hour
	performanceInterval    = time.Minute
	performanceMaxSamples  = 7 * 24 * 60
)

type PerformanceSample struct {
	Timestamp             int64   `json:"ts"`
	UptimeSeconds         int64   `json:"uptime_sec"`
	HostCPUPercent        float64 `json:"host_cpu_pct"`
	ProcessCPUPercent     float64 `json:"process_cpu_pct"`
	Load1                 float64 `json:"load_1"`
	ProcessRSSBytes       uint64  `json:"process_rss_bytes"`
	GoAllocBytes          uint64  `json:"go_alloc_bytes"`
	GoSysBytes            uint64  `json:"go_sys_bytes"`
	SystemMemoryBytes     uint64  `json:"system_memory_bytes"`
	AvailableMemoryBytes  uint64  `json:"available_memory_bytes"`
	DiskTotalBytes        uint64  `json:"disk_total_bytes"`
	DiskFreeBytes         uint64  `json:"disk_free_bytes"`
	Goroutines            int     `json:"goroutines"`
	WebSocketClients      int     `json:"websocket_clients"`
	TCPClients            int     `json:"tcp_clients"`
	MQTTConnections       int64   `json:"mqtt_connections"`
	MQTTConnectionsTotal  int64   `json:"mqtt_connections_total"`
	MQTTAuthFailuresTotal int64   `json:"mqtt_auth_failures_total"`
	MQTTAuthFailuresPM    float64 `json:"mqtt_auth_failures_per_min"`
	MQTTPublishesTotal    int64   `json:"mqtt_publishes_total"`
	MQTTRejectedTotal     int64   `json:"mqtt_rejected_total"`
	PacketsRxTotal        uint64  `json:"packets_rx_total"`
	PacketsDroppedTotal   uint64  `json:"packets_dropped_total"`
	PacketsPerSecond      float64 `json:"packets_per_second"`
	DroppedPerSecond      float64 `json:"dropped_per_second"`
	BroadcastQueue        int     `json:"broadcast_queue"`
	BroadcastQueueCap     int     `json:"broadcast_queue_cap"`
	UpstreamQueue         int     `json:"upstream_queue"`
	UpstreamQueueCap      int     `json:"upstream_queue_cap"`
	UpstreamTxQueue       int     `json:"upstream_tx_queue"`
	UpstreamTxQueueCap    int     `json:"upstream_tx_queue_cap"`
	UpstreamConnected     bool    `json:"upstream_connected"`
}

type PerformanceIssue struct {
	Key       string  `json:"key"`
	Severity  string  `json:"severity"`
	Title     string  `json:"title"`
	Detail    string  `json:"detail"`
	StartedAt int64   `json:"started_at"`
	EndedAt   int64   `json:"ended_at,omitempty"`
	Active    bool    `json:"active"`
	Peak      float64 `json:"peak,omitempty"`
	Unit      string  `json:"unit,omitempty"`
}

type performanceResponse struct {
	Health         string              `json:"health"`
	Current        PerformanceSample   `json:"current"`
	History        []PerformanceSample `json:"history"`
	Issues         []PerformanceIssue  `json:"issues"`
	Range          string              `json:"range"`
	RetentionHours int                 `json:"retention_hours"`
	SampleInterval int                 `json:"sample_interval_sec"`
}

var (
	performanceMu      sync.RWMutex
	performanceHistory []PerformanceSample
	perfPrevCPU        cpuCounters
	perfPersistCount   int
)

type cpuCounters struct {
	hostTotal uint64
	hostIdle  uint64
	procTicks uint64
	valid     bool
}

func initPerformanceMonitoring() {
	loadPerformanceHistory()
	collectPerformanceSample(true)
	go func() {
		ticker := time.NewTicker(performanceInterval)
		defer ticker.Stop()
		for range ticker.C {
			collectPerformanceSample(true)
		}
	}()
}

func collectPerformanceSample(persist bool) PerformanceSample {
	now := time.Now()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	s := PerformanceSample{
		Timestamp:             now.Unix(),
		UptimeSeconds:         int64(time.Since(metrics.StartTime).Seconds()),
		Load1:                 readLoad1(),
		ProcessRSSBytes:       readProcessRSS(),
		GoAllocBytes:          ms.Alloc,
		GoSysBytes:            ms.Sys,
		Goroutines:            runtime.NumGoroutine(),
		MQTTConnections:       atomic.LoadInt64(&mqttActiveConnections),
		MQTTConnectionsTotal:  atomic.LoadInt64(&mqttConnectionsAccepted),
		MQTTAuthFailuresTotal: atomic.LoadInt64(&mqttAuthFailures),
		MQTTPublishesTotal:    atomic.LoadInt64(&mqttPublishes),
		MQTTRejectedTotal:     atomic.LoadInt64(&mqttConnectionsRejected),
		BroadcastQueue:        len(broadcast),
		BroadcastQueueCap:     cap(broadcast),
		UpstreamQueue:         len(upstreamOut),
		UpstreamQueueCap:      cap(upstreamOut),
		UpstreamTxQueue:       len(upstreamTx),
		UpstreamTxQueueCap:    cap(upstreamTx),
		UpstreamConnected:     atomic.LoadInt32(&upstreamConnected) == 1,
	}
	s.SystemMemoryBytes, s.AvailableMemoryBytes = readSystemMemory()
	s.DiskTotalBytes, s.DiskFreeBytes = readDiskUsage()
	s.HostCPUPercent, s.ProcessCPUPercent = readCPUPercent()

	metrics.RLock()
	s.PacketsRxTotal = metrics.PktsRx
	s.PacketsDroppedTotal = metrics.PktsDropped
	metrics.RUnlock()

	clientsMu.Lock()
	s.WebSocketClients = len(clients)
	clientsMu.Unlock()
	tcpClientsMu.Lock()
	s.TCPClients = len(tcpClients)
	tcpClientsMu.Unlock()

	performanceMu.Lock()
	if n := len(performanceHistory); n > 0 {
		prev := performanceHistory[n-1]
		elapsed := float64(s.Timestamp - prev.Timestamp)
		if elapsed > 0 {
			if s.PacketsRxTotal >= prev.PacketsRxTotal {
				s.PacketsPerSecond = float64(s.PacketsRxTotal-prev.PacketsRxTotal) / elapsed
			}
			if s.PacketsDroppedTotal >= prev.PacketsDroppedTotal {
				s.DroppedPerSecond = float64(s.PacketsDroppedTotal-prev.PacketsDroppedTotal) / elapsed
			}
			if s.MQTTAuthFailuresTotal >= prev.MQTTAuthFailuresTotal {
				s.MQTTAuthFailuresPM = float64(s.MQTTAuthFailuresTotal-prev.MQTTAuthFailuresTotal) * 60 / elapsed
			}
		}
	}
	performanceHistory = append(performanceHistory, s)
	cutoff := now.Add(-performanceRetention).Unix()
	first := sort.Search(len(performanceHistory), func(i int) bool {
		return performanceHistory[i].Timestamp >= cutoff
	})
	if first > 0 {
		performanceHistory = append([]PerformanceSample(nil), performanceHistory[first:]...)
	}
	if len(performanceHistory) > performanceMaxSamples {
		performanceHistory = append([]PerformanceSample(nil), performanceHistory[len(performanceHistory)-performanceMaxSamples:]...)
	}
	perfPersistCount++
	compact := perfPersistCount%1440 == 0
	performanceMu.Unlock()

	if persist {
		appendPerformanceSample(s)
		if compact {
			compactPerformanceHistory()
		}
	}
	return s
}

func loadPerformanceHistory() {
	f, err := os.Open(performanceHistoryFile)
	if err != nil {
		return
	}
	defer f.Close()
	cutoff := time.Now().Add(-performanceRetention).Unix()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var loaded []PerformanceSample
	for scanner.Scan() {
		var s PerformanceSample
		if json.Unmarshal(scanner.Bytes(), &s) == nil && s.Timestamp >= cutoff {
			loaded = append(loaded, s)
		}
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Timestamp < loaded[j].Timestamp })
	if len(loaded) > performanceMaxSamples {
		loaded = loaded[len(loaded)-performanceMaxSamples:]
	}
	performanceMu.Lock()
	performanceHistory = loaded
	performanceMu.Unlock()
	if err := scanner.Err(); err != nil {
		logPerformanceError("read", err)
	}
}

func appendPerformanceSample(s PerformanceSample) {
	f, err := os.OpenFile(performanceHistoryFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logPerformanceError("append", err)
		return
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(s); err != nil {
		logPerformanceError("append", err)
	}
}

func compactPerformanceHistory() {
	performanceMu.RLock()
	snapshot := append([]PerformanceSample(nil), performanceHistory...)
	performanceMu.RUnlock()
	tmp := performanceHistoryFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		logPerformanceError("compact", err)
		return
	}
	enc := json.NewEncoder(f)
	for _, s := range snapshot {
		if err := enc.Encode(s); err != nil {
			f.Close()
			os.Remove(tmp)
			logPerformanceError("compact", err)
			return
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		logPerformanceError("compact", err)
		return
	}
	if err := os.Rename(tmp, performanceHistoryFile); err != nil {
		os.Remove(tmp)
		logPerformanceError("compact", err)
	}
}

func logPerformanceError(action string, err error) {
	fmt.Printf("performance history %s failed: %v\n", action, err)
}

func handleAdminPerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	rangeName := r.URL.Query().Get("range")
	duration := map[string]time.Duration{
		"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": performanceRetention,
	}[rangeName]
	if duration == 0 {
		rangeName, duration = "24h", 24*time.Hour
	}
	performanceMu.RLock()
	all := append([]PerformanceSample(nil), performanceHistory...)
	performanceMu.RUnlock()
	cutoff := time.Now().Add(-duration).Unix()
	first := sort.Search(len(all), func(i int) bool { return all[i].Timestamp >= cutoff })
	selected := all[first:]
	selected = downsamplePerformance(selected, 720)

	current := PerformanceSample{}
	if len(all) > 0 {
		current = all[len(all)-1]
	}
	issues := buildPerformanceIssues(all[first:])
	health := "healthy"
	for _, issue := range issues {
		if !issue.Active {
			continue
		}
		if issue.Severity == "critical" {
			health = "critical"
			break
		}
		if issue.Severity == "warning" {
			health = "warning"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(performanceResponse{
		Health:         health,
		Current:        current,
		History:        selected,
		Issues:         issues,
		Range:          rangeName,
		RetentionHours: int(performanceRetention.Hours()),
		SampleInterval: int(performanceInterval.Seconds()),
	})
}

func downsamplePerformance(samples []PerformanceSample, max int) []PerformanceSample {
	if len(samples) <= max || max < 2 {
		return append([]PerformanceSample(nil), samples...)
	}
	out := make([]PerformanceSample, 0, max)
	step := float64(len(samples)-1) / float64(max-1)
	last := -1
	for i := 0; i < max; i++ {
		idx := int(float64(i) * step)
		if idx != last {
			out = append(out, samples[idx])
			last = idx
		}
	}
	if out[len(out)-1].Timestamp != samples[len(samples)-1].Timestamp {
		out[len(out)-1] = samples[len(samples)-1]
	}
	return out
}

type issueCondition struct {
	key, severity, title, detail, unit string
	value                              float64
}

func sampleConditions(s PerformanceSample) []issueCondition {
	var out []issueCondition
	add := func(ok bool, key, severity, title, detail, unit string, value float64) {
		if ok {
			out = append(out, issueCondition{key, severity, title, detail, unit, value})
		}
	}
	add(s.HostCPUPercent >= 95, "host_cpu", "critical", "Host CPU critically high", "Host CPU has reached at least 95%.", "%", s.HostCPUPercent)
	if s.HostCPUPercent >= 85 && s.HostCPUPercent < 95 {
		add(true, "host_cpu", "warning", "Host CPU high", "Host CPU has reached at least 85%.", "%", s.HostCPUPercent)
	}
	if s.SystemMemoryBytes > 0 {
		avail := float64(s.AvailableMemoryBytes) * 100 / float64(s.SystemMemoryBytes)
		add(avail <= 5, "memory", "critical", "System memory critically low", "Available system memory is 5% or less.", "% available", avail)
		if avail > 5 {
			add(avail <= 15, "memory", "warning", "System memory low", "Available system memory is 15% or less.", "% available", avail)
		}
	}
	if s.DiskTotalBytes > 0 {
		used := float64(s.DiskTotalBytes-s.DiskFreeBytes) * 100 / float64(s.DiskTotalBytes)
		add(used >= 95, "disk", "critical", "Disk critically full", "Root disk usage is at least 95%.", "% used", used)
		if used < 95 {
			add(used >= 85, "disk", "warning", "Disk space low", "Root disk usage is at least 85%.", "% used", used)
		}
	}
	maxQueue := maxQueuePercent(s)
	add(maxQueue >= 90, "queue", "critical", "Internal queue nearly full", "One or more packet queues are at least 90% full.", "%", maxQueue)
	if maxQueue < 90 {
		add(maxQueue >= 70, "queue", "warning", "Internal queue pressure", "One or more packet queues are at least 70% full.", "%", maxQueue)
	}
	add(!s.UpstreamConnected && s.UptimeSeconds > 90, "upstream", "warning", "APRS-IS upstream disconnected", "The server is not currently connected to its APRS-IS upstream.", "", 0)
	add(s.MQTTAuthFailuresPM >= 30, "mqtt_auth", "critical", "MQTT authentication storm", "At least 30 MQTT logins failed during the last minute.", "/min", s.MQTTAuthFailuresPM)
	if s.MQTTAuthFailuresPM < 30 {
		add(s.MQTTAuthFailuresPM >= 1, "mqtt_auth", "warning", "MQTT authentication failures", "One or more MQTT logins failed during the last minute. Check the device username and member/device password.", "/min", s.MQTTAuthFailuresPM)
	}
	add(s.Goroutines >= 5000, "goroutines", "critical", "Very high goroutine count", "The process has at least 5,000 goroutines.", "", float64(s.Goroutines))
	if s.Goroutines < 5000 {
		add(s.Goroutines >= 1000, "goroutines", "warning", "High goroutine count", "The process has at least 1,000 goroutines.", "", float64(s.Goroutines))
	}
	return out
}

func buildPerformanceIssues(samples []PerformanceSample) []PerformanceIssue {
	type activeIssue struct {
		PerformanceIssue
		lastSeen int64
	}
	active := make(map[string]*activeIssue)
	var completed []PerformanceIssue
	for i, s := range samples {
		if i > 0 && s.UptimeSeconds < samples[i-1].UptimeSeconds {
			completed = append(completed, PerformanceIssue{
				Key: "restart", Severity: "info", Title: "Service restarted",
				Detail: "The APRS service uptime counter restarted.", StartedAt: s.Timestamp, EndedAt: s.Timestamp,
			})
		}
		seen := make(map[string]bool)
		for _, c := range sampleConditions(s) {
			seen[c.key] = true
			cur := active[c.key]
			if cur == nil {
				active[c.key] = &activeIssue{PerformanceIssue: PerformanceIssue{
					Key: c.key, Severity: c.severity, Title: c.title, Detail: c.detail,
					StartedAt: s.Timestamp, Active: true, Peak: c.value, Unit: c.unit,
				}, lastSeen: s.Timestamp}
				continue
			}
			cur.lastSeen = s.Timestamp
			if c.severity == "critical" {
				cur.Severity, cur.Title, cur.Detail = c.severity, c.title, c.detail
			}
			if c.key == "memory" {
				if cur.Peak == 0 || c.value < cur.Peak {
					cur.Peak = c.value
				}
			} else if c.value > cur.Peak {
				cur.Peak = c.value
			}
		}
		for key, cur := range active {
			if !seen[key] {
				cur.Active = false
				cur.EndedAt = cur.lastSeen
				completed = append(completed, cur.PerformanceIssue)
				delete(active, key)
			}
		}
	}
	for _, cur := range active {
		completed = append(completed, cur.PerformanceIssue)
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].Active != completed[j].Active {
			return completed[i].Active
		}
		return completed[i].StartedAt > completed[j].StartedAt
	})
	if len(completed) > 100 {
		completed = completed[:100]
	}
	return completed
}

func maxQueuePercent(s PerformanceSample) float64 {
	pct := func(n, cap int) float64 {
		if cap == 0 {
			return 0
		}
		return float64(n) * 100 / float64(cap)
	}
	return maxFloat(pct(s.BroadcastQueue, s.BroadcastQueueCap),
		pct(s.UpstreamQueue, s.UpstreamQueueCap),
		pct(s.UpstreamTxQueue, s.UpstreamTxQueueCap))
}

func maxFloat(v ...float64) float64 {
	var m float64
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func readLoad1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func readProcessRSS() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func readSystemMemory() (total, available uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	return
}

func readDiskUsage() (total, free uint64) {
	out, err := exec.Command("df", "-Pk", "/").Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, 0
	}
	totalKB, _ := strconv.ParseUint(fields[1], 10, 64)
	freeKB, _ := strconv.ParseUint(fields[3], 10, 64)
	return totalKB * 1024, freeKB * 1024
}

func readCPUPercent() (host, process float64) {
	cur := readCPUCounters()
	if !cur.valid {
		return 0, 0
	}
	prev := perfPrevCPU
	perfPrevCPU = cur
	if !prev.valid || cur.hostTotal <= prev.hostTotal {
		return 0, 0
	}
	totalDelta := cur.hostTotal - prev.hostTotal
	idleDelta := cur.hostIdle - prev.hostIdle
	if idleDelta <= totalDelta {
		host = float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
	}
	if cur.procTicks >= prev.procTicks {
		process = float64(cur.procTicks-prev.procTicks) * float64(runtime.NumCPU()) * 100 / float64(totalDelta)
	}
	return
}

func readCPUCounters() cpuCounters {
	var out cpuCounters
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return out
	}
	line := strings.SplitN(string(stat), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return out
	}
	var values []uint64
	for _, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuCounters{}
		}
		values = append(values, v)
		out.hostTotal += v
	}
	out.hostIdle = values[3]
	if len(values) > 4 {
		out.hostIdle += values[4]
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return cpuCounters{}
	}
	closeParen := strings.LastIndexByte(string(self), ')')
	if closeParen < 0 {
		return cpuCounters{}
	}
	selfFields := strings.Fields(string(self)[closeParen+1:])
	if len(selfFields) < 13 {
		return cpuCounters{}
	}
	utime, err1 := strconv.ParseUint(selfFields[11], 10, 64)
	stime, err2 := strconv.ParseUint(selfFields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return cpuCounters{}
	}
	out.procTicks = utime + stime
	out.valid = true
	return out
}
