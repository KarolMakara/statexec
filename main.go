package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blackswifthosting/statexec/collectors"
)

var (
	version        = "dev"
	jobName string = "statexec"

	metricsFile              string = ""
	metricsStartTimeOverride int64  = -1 // in milliseconds
	delayBeforeCommand       int64  = 0
	delayAfterCommand        int64  = 0
	instanceOverride         string = ""

	role            string = "standalone"
	serverIp        string = ""
	syncPort        string = "8080"
	syncWaitForStop bool   = true

	extraLabels map[string]string

	metricsStartTime int64 // in milliseconds
	instance         string
	commandState     int = 0
)

const (
	EnvVarPrefix         = "SE_"
	MetricPrefix         = "statexec_"
	CommandStatusPending = 0
	CommandStatusRunning = 1
	CommandStatusDone    = 2

	ModeStandalone = 0
	ModeLeader     = 1
	ModeFollower   = 2
)

func main() {
	// Default values
	metricsFile = jobName + "_metrics.prom"
	extraLabels = make(map[string]string)

	// Parse environment variables
	parseEnvVars()

	cmd := parseArgs()

	if instanceOverride != "" {
		instance = instanceOverride
	} else {
		instance = cmd[0]
	}

	// Delete metrics file if it exists
	_ = os.Remove(metricsFile)

	fmt.Println("Command: " + strings.Join(cmd, " "))

	fmt.Printf("Metrics file: %s\n", metricsFile)
	fmt.Printf("Instance: %s\n", instance)
	fmt.Printf("Metrics start time: %d\n", metricsStartTime)
	fmt.Printf("Delay before command: %d\n", delayBeforeCommand)
	fmt.Printf("Delay after command: %d\n", delayAfterCommand)
	fmt.Printf("Role: %s\n", role)
	fmt.Printf("Server IP: %s\n", serverIp)
	fmt.Printf("Sync port: %s\n", syncPort)
	fmt.Printf("Sync wait for stop: %v\n", syncWaitForStop)
	fmt.Printf("Extra labels: %v\n", extraLabels)

	execCmd := exec.Command(cmd[0], cmd[1:]...)

	switch role {
	case "standalone":
		fmt.Println("Starting statexec in standalone mode")
		startCommand(execCmd)
	case "client":
		fmt.Printf("Starting statexec as client of http://%s:%s (withstop : %v)", serverIp, syncPort, syncWaitForStop)
		syncStartCommand(execCmd, fmt.Sprintf("http://%s:%s", serverIp, syncPort), syncWaitForStop)
	case "server":
		fmt.Printf("Starting statexec as server on port %s (withstop : %v)", syncPort, syncWaitForStop)
		waitForHttpSyncToStartCommand(execCmd, syncWaitForStop)
	}
}

func usage() {
	binself := os.Args[0]
	fmt.Printf("Usage: %s [OPTIONS] <command> [command args]\n", binself)
	fmt.Printf("Version: %s\n", version)
	fmt.Println("")
	fmt.Println("Common options:")
	fmt.Printf("  --file, -f <file>                       %sFILE                 Metrics file (default: statexec_metrics.prom)\n", EnvVarPrefix)
	fmt.Printf("  --instance, -i <instance>               %sINSTANCE             Instance name (default: <command>)\n", EnvVarPrefix)
	fmt.Printf("  --metrics-start-time, -mst <timestamp>  %sMETRICS_START_TIME   Metrics start time in milliseconds (default: now)\n", EnvVarPrefix)
	fmt.Printf("  --delay, -d <seconds>                   %sDELAY                Delay in seconds before and after the command (default: 0)\n", EnvVarPrefix)
	fmt.Printf("  --delay-before-command, -dbc <seconds>  %sDELAY_BEFORE_COMMAND Delay in seconds  before the command (default: 0)\n", EnvVarPrefix)
	fmt.Printf("  --delay-after-command, -dac <seconds>   %sDELAY_AFTER_COMMAND  Delay in seconds  after the command (default: 0)\n", EnvVarPrefix)
	fmt.Printf("  --label, -l <key>=<value>               %sLABEL_<key>          Extra label to add to all metrics\n", EnvVarPrefix)
	fmt.Println("Synchronization options:")
	fmt.Printf("  --connect, -c <ip>                      %sCONNECT              Connect to server mode\n", EnvVarPrefix)
	fmt.Printf("  --server, -s                            %sSERVER               Start server mode\n", EnvVarPrefix)
	fmt.Printf("  --sync-port, -sp <port>                 %sSYNC_PORT            Sync port (default: 8080)\n", EnvVarPrefix)
	fmt.Printf("  --sync-start-only, -sso                 %sSYNC_START_ONLY      Sync start only (default: false)\n", EnvVarPrefix)
	fmt.Println("Other options:")
	fmt.Printf("  --version, -v        Print version and exit\n")
	fmt.Printf("  --help, -help, -h    Print help and exit\n")
	fmt.Printf("  --                   Stop parsing arguments\n")
	fmt.Println("")
	fmt.Println("Standalone examples:")
	fmt.Printf("  %s ping 8.8.8.8 -c 4\n", binself)
	fmt.Printf("  %sFILE=data.prom %sLABEL_type=sample %s -d 3 -l env=dev -- ./mycommand.sh arg1 arg2\n", EnvVarPrefix, EnvVarPrefix, binself)
	fmt.Println("")
	fmt.Println("Sync mode examples:")
	fmt.Println("  # Wait for a client sync to start the command")
	fmt.Printf("  %s -s -- date\n", binself)
	fmt.Println("  # Connect to server on <localhost> to start and stop the command")
	fmt.Printf("  %s -c localhost -- echo start date now\n", binself)
}

func appendToResultFile(text string) {
	// Open metrics file in append mode
	resultFile, err := os.OpenFile(metricsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("Error opening metrics file:", err)
		os.Exit(1)
	}
	defer resultFile.Close()
	if _, err := resultFile.WriteString(text); err != nil {
		fmt.Println("Error writing to metrics file:", err)
		os.Exit(1)
	}

}

func parseArgs() []string {
	var err error
	cmd := []string{}

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-f", "--file":
			metricsFile = os.Args[i+1]
			i++

		case "-i", "--instance":
			instanceOverride = os.Args[i+1]
			i++

		case "-mst", "--metrics-start-time":
			metricsStartTimeOverride, err = strconv.ParseInt(os.Args[i+1], 10, 64)
			if err != nil {
				fmt.Println("Error parsing metrics time override:", err)
				os.Exit(1)
			}
			i++

		case "-c", "--connect":
			if role == "server" {
				fmt.Println("Error: server and client modes are mutually exclusive")
				os.Exit(1)
			}
			role = "client"
			serverIp = os.Args[i+1]
			i++
		case "-s", "--server":
			if role == "client" {
				fmt.Println("Error: server and client modes are mutually exclusive")
				os.Exit(1)
			}
			role = "server"

		case "-sp", "--sync-port":
			syncPort = os.Args[i+1]
			i++
		case "-sso", "--sync-start-only":
			syncWaitForStop = false

		// Delay in seconds
		case "-d", "--delay":
			timeToWaitInScd, err := strconv.ParseInt(os.Args[i+1], 10, 64)
			if err != nil {
				fmt.Println("Error parsing wait time:", err)
				os.Exit(1)
			}
			delayBeforeCommand = timeToWaitInScd
			delayAfterCommand = timeToWaitInScd
			i++
		case "-dbc", "--delay-before-command":
			timeToWaitInMs, err := strconv.ParseInt(os.Args[i+1], 10, 64)
			if err != nil {
				fmt.Println("Error parsing wait time:", err)
				os.Exit(1)
			}
			delayBeforeCommand = timeToWaitInMs
			i++
		case "-dac", "--delay-after-command":
			timeToWaitInMs, err := strconv.ParseInt(os.Args[i+1], 10, 64)
			if err != nil {
				fmt.Println("Error parsing wait time:", err)
				os.Exit(1)
			}
			delayAfterCommand = timeToWaitInMs
			i++

		// Extra labels
		case "-l", "--label":
			parts := strings.SplitN(os.Args[i+1], "=", 2)
			if len(parts) == 2 {
				addLabel(parts[0], parts[1])
			} else {
				fmt.Println("Error parsing label:", os.Args[i+1])
				os.Exit(1)
			}
			i++

		case "-v", "--version":
			fmt.Println(version)
			os.Exit(0)
		case "-h", "-help", "--help":
			usage()
			os.Exit(0)
		case "--":
			cmd = os.Args[i+1:]
			i = len(os.Args)
		default:
			cmd = os.Args[i:]
			i = len(os.Args)
		}
	}
	return cmd
}

func parseEnvVars() {
	var err error
	// Metrics file (-f, --file)
	if value := os.Getenv(EnvVarPrefix + "FILE"); value != "" {
		metricsFile = value
	}

	// Instance name (-i, --instance)
	if value := os.Getenv(EnvVarPrefix + "INSTANCE"); value != "" {
		instanceOverride = value
	}

	// Metrics start time (-mst, --metrics-start-time)
	if value := os.Getenv(EnvVarPrefix + "METRICS_START_TIME"); value != "" {
		metricsStartTimeOverride, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			fmt.Println("Error parsing "+EnvVarPrefix+"METRICS_START_TIME env var, must be an int64 (timestamp in ms since epoch), found : ", value)
			os.Exit(1)
		}
	}

	// Connect to server (-c, --connect)
	if value := os.Getenv(EnvVarPrefix + "CONNECT"); value != "" {
		if role == "server" {
			fmt.Println("Error: server and client modes are mutually exclusive")
			os.Exit(1)
		}
		role = "client"
		serverIp = value
	}

	// Start server (-s, --server)
	if value := os.Getenv(EnvVarPrefix + "SERVER"); value != "" {
		if role == "client" {
			fmt.Println("Error: server and client modes are mutually exclusive")
			os.Exit(1)
		}
		role = "server"
	}

	// Sync port (-sp, --sync-port)
	if value := os.Getenv(EnvVarPrefix + "SYNC_PORT"); value != "" {
		syncPort = value
	}

	// Sync start only (-sso, --sync-start-only)
	if value := os.Getenv(EnvVarPrefix + "SYNC_START_ONLY"); value != "" {
		syncWaitForStop = false
	}

	// Delay in seconds (-d, --delay)
	if value := os.Getenv(EnvVarPrefix + "DELAY"); value != "" {
		timeToWaitInScd, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			fmt.Println("Error parsing "+EnvVarPrefix+"DELAY env var, must be an int64 (time in ms), found : ", value)
			os.Exit(1)
		}
		delayBeforeCommand = timeToWaitInScd
		delayAfterCommand = timeToWaitInScd
	}

	// Delay before command in seconds (-dbc, --delay-before-command)
	if value := os.Getenv(EnvVarPrefix + "DELAY_BEFORE_COMMAND"); value != "" {
		timeToWaitInScd, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			fmt.Println("Error parsing "+EnvVarPrefix+"DELAY_BEFORE_COMMAND env var, must be an int64 (time in ms), found : ", value)
			os.Exit(1)
		}
		delayBeforeCommand = timeToWaitInScd
	}

	// Delay after command in seconds (-dac, --delay-after-command)
	if value := os.Getenv(EnvVarPrefix + "DELAY_AFTER_COMMAND"); value != "" {
		timeToWaitInScd, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			fmt.Println("Error parsing "+EnvVarPrefix+"DELAY_AFTER_COMMAND env var, must be an int64 (time in ms), found : ", value)
			os.Exit(1)
		}
		delayAfterCommand = timeToWaitInScd
	}

	// Get extra labels from environment variables (-l, --label)
	parseExtraLabelsFromEnv()
}

func addLabel(key string, value string) {
	// List of forbidden label names
	forbiddenKeys := []string{"instance", "job", "cpu", "mode", "interface"}

	// Replace non-alphanumeric characters with underscores
	safeKey := regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(key, "_")

	// Check if key is not forbidden
	for _, forbiddenKey := range forbiddenKeys {
		if safeKey == forbiddenKey {
			fmt.Printf("Override label %s is forbidden", key)
			os.Exit(1)
		}
	}

	extraLabels[strings.ToLower(safeKey)] = value
}

func parseExtraLabelsFromEnv() map[string]string {
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, EnvVarPrefix+"LABEL_") {
			parts := strings.Split(env, "=")
			if len(parts) == 2 {
				key := strings.TrimPrefix(parts[0], EnvVarPrefix+"LABEL_")
				value := parts[1]
				addLabel(key, value)
			} else {
				fmt.Println("Error parsing label of ENV :", env)
				os.Exit(1)
			}
		}
	}
	return extraLabels
}

func syncStartCommand(cmd *exec.Cmd, syncServerUrl string, syncStop bool) {

	fmt.Println("Sending start sync at " + syncServerUrl + "/start")
	_, err := http.Post(syncServerUrl+"/start", "text/plain", nil)
	if err != nil {
		fmt.Println("Error sending start sync request:", err)
		os.Exit(1)
	}
	fmt.Println("Start sync done")

	startCommand(cmd)

	if syncStop {
		fmt.Println("Sending stop sync at " + syncServerUrl + "/stop")
		_, err := http.Post(syncServerUrl+"/stop", "text/plain", nil)
		if err != nil {
			fmt.Println("Error sending stop sync request:", err)
			os.Exit(1)
		}
		fmt.Println("Command finished sync ")
	}
}

func waitForHttpSyncToStartCommand(cmd *exec.Cmd, waitForStop bool) {
	// Create mutex
	var mutex = &sync.Mutex{}
	var cmdStarted = false
	var cmdFinished = false

	server := &http.Server{
		Addr: ":8080",
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><a href="/start">/start</a> : Start the command</body></html>`)
	})

	http.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()

		if cmdStarted {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, "KO")
		} else {

			// Start the command in a goroutine
			go func() {
				cmdStarted = true
				startCommand(cmd)
				cmdFinished = true

				if !waitForStop {
					os.Exit(0)
				}
			}()

			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, "OK")
		}
	})

	http.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if cmdStarted {
			if cmdFinished {
				w.WriteHeader(http.StatusNoContent)
				fmt.Fprintf(w, "Command already finished")
			} else {
				w.WriteHeader(http.StatusAccepted)
				cmd.Process.Signal(os.Interrupt)
				fmt.Fprintf(w, "Command stopped")
			}

			go func() {
				// Create a context with a timeout
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				// Shutdown the server gracefully
				if err := server.Shutdown(ctx); err != nil {
					panic(err)
				}
			}()

		} else {
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprintf(w, "Command not started yet")
		}
	})
	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		fmt.Println("Error starting the server:", err)
	}
}

type GrafanaAnnotation struct {
	Time    int64    `json:"time"`
	TimeEnd int64    `json:"timeEnd"`
	Text    string   `json:"text"`
	Tags    []string `json:"tags"`
}

func writeAnnotation(annotation GrafanaAnnotation) {
	annotationBuffer, err := json.Marshal(annotation)
	if err != nil {
		fmt.Println("Error marshalling annotation:", err)
		os.Exit(1)
	}
	appendToResultFile("#grafana-annotation " + string(annotationBuffer) + "\n")
}

func startCommand(cmd *exec.Cmd) {
	var err error
	var wg sync.WaitGroup

	realStartTime := time.Now()

	if metricsStartTimeOverride != -1 {
		metricsStartTime = metricsStartTimeOverride
	} else {
		metricsStartTime = realStartTime.UnixMilli()
	}

	// Connect the command's standard input/output/error to those of the program
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Channel to signal when to stop gathering metrics
	quit := make(chan struct{})
	defer close(quit)

	// Start gathering metrics in a goroutine we will wait for
	wg.Add(1)
	go func() {
		defer wg.Done()
		startGathering(quit)
	}()

	// Wait before starting the command
	if delayBeforeCommand > 0 {
		time.Sleep(time.Duration(delayBeforeCommand) * time.Second)
	}

	// Catch interrupt signal and forward it to the child process
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		sig := <-sigs
		// Transmettre le signal SIGINT au processus enfant
		if err := cmd.Process.Signal(sig); err != nil {
			panic(err)
		}
	}()

	// Start the command
	err = cmd.Start()
	if err != nil {
		fmt.Println("Error starting command:", err)
		os.Exit(1)
	}

	commandState = CommandStatusRunning

	// Write annotation
	annotationTime := metricsStartTime + time.Now().UnixMilli() - realStartTime.UnixMilli()
	writeAnnotation(GrafanaAnnotation{
		Time:    annotationTime,
		TimeEnd: annotationTime,
		Text:    "Command started",
		Tags: []string{
			"statexec",
			"start",
			"instance=" + instance,
			"job=" + jobName,
			"role=" + role,
		},
	})

	// Wait for the command to finish
	_ = cmd.Wait()

	commandState = CommandStatusDone

	// Write annotation
	annotationTime = metricsStartTime + time.Now().UnixMilli() - realStartTime.UnixMilli()
	writeAnnotation(GrafanaAnnotation{
		Time:    annotationTime,
		TimeEnd: annotationTime,
		Text:    "Command done",
		Tags: []string{
			"statexec",
			"done",
			"instance=" + instance,
			"job=" + jobName,
			"role=" + role,
		},
	})

	// Wait after the command
	if delayAfterCommand > 0 {
		time.Sleep(time.Duration(delayAfterCommand) * time.Second)
	}

	// Signal to stop gathering metrics
	stopGatheringMetrics(quit)

	// Wait for the metrics goroutine to finish
	wg.Wait()
}

// Start gathering metrics with a 1 second interval
func startGathering(quit chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	secondesSinceGatheringStart := 0

	gatherMetrics(secondesSinceGatheringStart)

	stopGatheringNextIteration := false
	for {
		select {
		case <-ticker.C:
			secondesSinceGatheringStart++
			gatherMetrics(secondesSinceGatheringStart)
			if stopGatheringNextIteration {
				return
			}
		case <-quit:
			stopGatheringNextIteration = true
		}
	}
}

func stopGatheringMetrics(quit chan struct{}) {
	quit <- struct{}{}
}

// Generate a string to render labels in prometheus format
func generateLabelRender(metricsLabels map[string]string) string {
	var labelRender []string

	// Static labels
	labelRender = append(labelRender, fmt.Sprintf("instance=\"%s\"", instance))
	labelRender = append(labelRender, fmt.Sprintf("job=\"%s\"", jobName))
	labelRender = append(labelRender, fmt.Sprintf("role=\"%s\"", role))

	// Metrics labels
	for key, value := range metricsLabels {
		labelRender = append(labelRender, fmt.Sprintf("%s=\"%s\"", key, value))
	}

	// Extra labels
	for key, value := range extraLabels {
		labelRender = append(labelRender, fmt.Sprintf("%s=\"%s\"", key, value))
	}
	return strings.Join(labelRender, ",")
}

// Gather metrics
func gatherMetrics(secondesSinceStart int) error {
	timeBeforeGathering := time.Now()
	metricsBuffer := ""
	defaultLabels := generateLabelRender(nil)
	currentTimestamp := metricsStartTime + int64(secondesSinceStart)*1000

	// Command status

	metricsBuffer += fmt.Sprintf(MetricPrefix+"command_status{%s} %d %d\n", defaultLabels, commandState, currentTimestamp)

	// CPU usage

	cpuMetrics := collectors.CollectCpuMetrics()
	for _, cpuMetric := range cpuMetrics {
		for mode, cpuTime := range cpuMetric.CpuTimePerMode {
			metricLabels := map[string]string{
				"cpu":  cpuMetric.Cpu,
				"mode": mode,
			}
			metricsBuffer += fmt.Sprintf(MetricPrefix+"cpu_seconds_total{%s} %f %d\n", generateLabelRender(metricLabels), cpuTime, currentTimestamp)
		}
	}

	// Memory usage

	memoryMetrics := collectors.CollectMemoryMetrics()
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_total_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Total, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_available_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Available, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_used_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Used, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_free_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Free, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_buffers_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Buffers, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_cached_bytes{%s} %d %d\n", defaultLabels, memoryMetrics.Cached, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"memory_used_percent{%s} %f %d\n", defaultLabels, memoryMetrics.UsedPercent, currentTimestamp)

	// Network counters

	networkMetrics := collectors.CollectNetworkMetrics()
	for _, networkMetric := range networkMetrics {
		metricLabels := map[string]string{
			"interface": networkMetric.Interface,
		}
		metricsBuffer += fmt.Sprintf(MetricPrefix+"network_sent_bytes_total{%s} %d %d\n", generateLabelRender(metricLabels), networkMetric.SentTotalBytes, currentTimestamp)
		metricsBuffer += fmt.Sprintf(MetricPrefix+"network_received_bytes_total{%s} %d %d\n", generateLabelRender(metricLabels), networkMetric.RecvTotalBytes, currentTimestamp)
	}

	// Disk monitoring

	diskMetrics := collectors.CollectDiskMetrics()
	for _, diskMetric := range diskMetrics {
		metricLabels := map[string]string{
			"disk": diskMetric.Device,
		}
		renderedLabels := generateLabelRender(metricLabels)
		metricsBuffer += fmt.Sprintf(MetricPrefix+"disk_read_bytes_total{%s} %d %d\n", renderedLabels, diskMetric.ReadBytesTotal, currentTimestamp)
		metricsBuffer += fmt.Sprintf(MetricPrefix+"disk_write_bytes_total{%s} %d %d\n", renderedLabels, diskMetric.WriteBytesTotal, currentTimestamp)
	}

	// Self monitoring
	metricsBuffer += fmt.Sprintf(MetricPrefix+"seconds_since_start{%s} %d %d\n", defaultLabels, secondesSinceStart, currentTimestamp)
	metricsBuffer += fmt.Sprintf(MetricPrefix+"metric_generation_duration_ms{%s} %d %d\n", defaultLabels, time.Since(timeBeforeGathering).Abs().Milliseconds(), currentTimestamp)

	// Write metrics to file
	appendToResultFile(metricsBuffer)

	return nil
}
