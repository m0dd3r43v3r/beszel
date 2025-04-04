package agent

import (
	"beszel/internal/entities/system"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/slog"
)

const (
	// Commands
	nvidiaSmiCmd  = "nvidia-smi"
	rocmSmiCmd    = "rocm-smi"
	tegraStatsCmd = "tegrastats"
	intelGpuTopCmd = "/usr/bin/intel_gpu_top"

	// Polling intervals
	nvidiaSmiInterval  = "4"    // in seconds
	tegraStatsInterval = "3700" // in milliseconds
	rocmSmiInterval    = 4300 * time.Millisecond
	intelGpuTopInterval = 4 * time.Second

	// Command retry and timeout constants
	retryWaitTime     = 5 * time.Second
	maxFailureRetries = 5

	cmdBufferSize = 10 * 1024

	// Unit Conversions
	mebibytesInAMegabyte = 1.024  // nvidia-smi reports memory in MiB
	milliwattsInAWatt    = 1000.0 // tegrastats reports power in mW
)

// GPUManager manages data collection for GPUs (either Nvidia or AMD)
type GPUManager struct {
	sync.Mutex
	nvidiaSmi    bool
	rocmSmi      bool
	tegrastats   bool
	intelGpuTop  bool
	GpuDataMap   map[string]*system.GPUData
}

// RocmSmiJson represents the JSON structure of rocm-smi output
type RocmSmiJson struct {
	ID           string `json:"GUID"`
	Name         string `json:"Card series"`
	Temperature  string `json:"Temperature (Sensor edge) (C)"`
	MemoryUsed   string `json:"VRAM Total Used Memory (B)"`
	MemoryTotal  string `json:"VRAM Total Memory (B)"`
	Usage        string `json:"GPU use (%)"`
	PowerPackage string `json:"Average Graphics Package Power (W)"`
	PowerSocket  string `json:"Current Socket Graphics Package Power (W)"`
}

// gpuCollector defines a collector for a specific GPU management utility (nvidia-smi or rocm-smi)
type gpuCollector struct {
	name    string
	cmdArgs []string
	parse   func([]byte) bool // returns true if valid data was found
	buf     []byte
}

var errNoValidData = fmt.Errorf("no valid GPU data found") // Error for missing data

// starts and manages the ongoing collection of GPU data for the specified GPU management utility
func (c *gpuCollector) start() {
	for {
		err := c.collect()
		if err != nil {
			if err == errNoValidData {
				slog.Warn(c.name + " found no valid GPU data, stopping")
				break
			}
			slog.Warn(c.name+" failed, restarting", "err", err)
			time.Sleep(retryWaitTime)
			continue
		}
	}
}

// collect executes the command, parses output with the assigned parser function
func (c *gpuCollector) collect() error {
	cmd := exec.Command(c.name, c.cmdArgs...)
	if c.name == intelGpuTopCmd {
		cmd.Path = intelGpuTopCmd // Use full path for intel_gpu_top
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// For intel_gpu_top, read the entire output as one JSON object
	if c.name == intelGpuTopCmd {
		output, err := io.ReadAll(stdout)
		if err != nil {
			return fmt.Errorf("failed to read intel_gpu_top output: %w", err)
		}
		if !c.parse(output) {
			return errNoValidData
		}
		return cmd.Wait()
	}

	// For other tools, use line-by-line scanning
	scanner := bufio.NewScanner(stdout)
	if c.buf == nil {
		c.buf = make([]byte, 0, cmdBufferSize)
	}
	scanner.Buffer(c.buf, bufio.MaxScanTokenSize)

	for scanner.Scan() {
		hasValidData := c.parse(scanner.Bytes())
		if !hasValidData {
			return errNoValidData
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}
	return cmd.Wait()
}

// getJetsonParser returns a function to parse the output of tegrastats and update the GPUData map
func (gm *GPUManager) getJetsonParser() func(output []byte) bool {
	// use closure to avoid recompiling the regex
	ramPattern := regexp.MustCompile(`RAM (\d+)/(\d+)MB`)
	gr3dPattern := regexp.MustCompile(`GR3D_FREQ (\d+)%`)
	tempPattern := regexp.MustCompile(`tj@(\d+\.?\d*)C`)
	// Orin Nano / NX do not have GPU specific power monitor
	// TODO: Maybe use VDD_IN for Nano / NX and add a total system power chart
	powerPattern := regexp.MustCompile(`(GPU_SOC|CPU_GPU_CV) (\d+)mW`)

	return func(output []byte) bool {
		gm.Lock()
		defer gm.Unlock()
		// we get gpu name from the intitial run of nvidia-smi, so return if it hasn't been initialized
		gpuData, ok := gm.GpuDataMap["0"]
		if !ok {
			return true
		}
		// Parse RAM usage
		ramMatches := ramPattern.FindSubmatch(output)
		if ramMatches != nil {
			gpuData.MemoryUsed, _ = strconv.ParseFloat(string(ramMatches[1]), 64)
			gpuData.MemoryTotal, _ = strconv.ParseFloat(string(ramMatches[2]), 64)
		}
		// Parse GR3D (GPU) usage
		gr3dMatches := gr3dPattern.FindSubmatch(output)
		if gr3dMatches != nil {
			gr3dUsage, _ := strconv.ParseFloat(string(gr3dMatches[1]), 64)
			gpuData.Usage += gr3dUsage
		}
		// Parse temperature
		tempMatches := tempPattern.FindSubmatch(output)
		if tempMatches != nil {
			gpuData.Temperature, _ = strconv.ParseFloat(string(tempMatches[1]), 64)
		}
		// Parse power usage
		powerMatches := powerPattern.FindSubmatch(output)
		if powerMatches != nil {
			power, _ := strconv.ParseFloat(string(powerMatches[2]), 64)
			gpuData.Power += power / milliwattsInAWatt
		}
		gpuData.Count++
		return true
	}
}

// parseNvidiaData parses the output of nvidia-smi and updates the GPUData map
func (gm *GPUManager) parseNvidiaData(output []byte) bool {
	gm.Lock()
	defer gm.Unlock()
	scanner := bufio.NewScanner(bytes.NewReader(output))
	var valid bool
	for scanner.Scan() {
		line := scanner.Text() // Or use scanner.Bytes() for []byte
		fields := strings.Split(strings.TrimSpace(line), ", ")
		if len(fields) < 7 {
			continue
		}
		valid = true
		id := fields[0]
		temp, _ := strconv.ParseFloat(fields[2], 64)
		memoryUsage, _ := strconv.ParseFloat(fields[3], 64)
		totalMemory, _ := strconv.ParseFloat(fields[4], 64)
		usage, _ := strconv.ParseFloat(fields[5], 64)
		power, _ := strconv.ParseFloat(fields[6], 64)
		// add gpu if not exists
		if _, ok := gm.GpuDataMap[id]; !ok {
			name := strings.TrimPrefix(fields[1], "NVIDIA ")
			gm.GpuDataMap[id] = &system.GPUData{Name: strings.TrimSuffix(name, " Laptop GPU")}
			// check if tegrastats is active - if so we will only use nvidia-smi to get gpu name
			// - nvidia-smi does not provide metrics for tegra / jetson devices
			// this will end the nvidia-smi collector
			if gm.tegrastats {
				return false
			}
		}
		// update gpu data
		gpu := gm.GpuDataMap[id]
		gpu.Temperature = temp
		gpu.MemoryUsed = memoryUsage / mebibytesInAMegabyte
		gpu.MemoryTotal = totalMemory / mebibytesInAMegabyte
		gpu.Usage += usage
		gpu.Power += power
		gpu.Count++
	}
	return valid
}

// parseAmdData parses the output of rocm-smi and updates the GPUData map
func (gm *GPUManager) parseAmdData(output []byte) bool {
	var rocmSmiInfo map[string]RocmSmiJson
	if err := json.Unmarshal(output, &rocmSmiInfo); err != nil || len(rocmSmiInfo) == 0 {
		return false
	}
	gm.Lock()
	defer gm.Unlock()
	for _, v := range rocmSmiInfo {
		var power float64
		if v.PowerPackage != "" {
			power, _ = strconv.ParseFloat(v.PowerPackage, 64)
		} else {
			power, _ = strconv.ParseFloat(v.PowerSocket, 64)
		}
		memoryUsage, _ := strconv.ParseFloat(v.MemoryUsed, 64)
		totalMemory, _ := strconv.ParseFloat(v.MemoryTotal, 64)
		usage, _ := strconv.ParseFloat(v.Usage, 64)

		if _, ok := gm.GpuDataMap[v.ID]; !ok {
			gm.GpuDataMap[v.ID] = &system.GPUData{Name: v.Name}
		}
		gpu := gm.GpuDataMap[v.ID]
		gpu.Temperature, _ = strconv.ParseFloat(v.Temperature, 64)
		gpu.MemoryUsed = bytesToMegabytes(memoryUsage)
		gpu.MemoryTotal = bytesToMegabytes(totalMemory)
		gpu.Usage += usage
		gpu.Power += power
		gpu.Count++
	}
	return true
}

// IntelGpuTopJson represents the JSON structure of intel-gpu-top output
type IntelGpuTopJson struct {
	Power struct {
		GPU     float64 `json:"GPU"`
		Package float64 `json:"Package"`
		Unit    string  `json:"unit"`
	} `json:"power"`
	Engines struct {
		Render3D struct {
			Busy float64 `json:"busy"`
			Unit string  `json:"unit"`
		} `json:"Render/3D"`
		Blitter struct {
			Busy float64 `json:"busy"`
			Unit string  `json:"unit"`
		} `json:"Blitter"`
		Video struct {
			Busy float64 `json:"busy"`
			Unit string  `json:"unit"`
		} `json:"Video"`
		VideoEnhance struct {
			Busy float64 `json:"busy"`
			Unit string  `json:"unit"`
		} `json:"VideoEnhance"`
	} `json:"engines"`
}

// getIntelGpuName gets the name of the Intel GPU using intel-gpu-top
func (gm *GPUManager) getIntelGpuName() string {
	cmd := exec.Command(intelGpuTopCmd)
	output, err := cmd.Output()
	if err != nil {
		return "Intel GPU"
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	if scanner.Scan() {
		line := scanner.Text()
		// Extract the GPU name from the first line
		// Format: "intel-gpu-top: Intel Alderlake_n (Gen12) @ /dev/dri/card1"
		if strings.Contains(line, "Intel") {
			parts := strings.Split(line, "Intel")
			if len(parts) > 1 {
				name := strings.Split(parts[1], "@")[0]
				name = strings.TrimSpace(name)
				return name
			}
		}
	}
	return "Intel GPU"
}

// parseIntelGpuTopData parses the output of intel-gpu-top and updates the GPUData map
func (gm *GPUManager) parseIntelGpuTopData(output []byte) bool {
	var intelGpuInfo IntelGpuTopJson
	if err := json.Unmarshal(output, &intelGpuInfo); err != nil {
		slog.Debug("Failed to parse Intel GPU JSON", "error", err)
		return false
	}
	gm.Lock()
	defer gm.Unlock()

	id := "0" // intel-gpu-top only shows one GPU at a time
	if _, ok := gm.GpuDataMap[id]; !ok {
		name := gm.getIntelGpuName()
		slog.Debug("Initializing Intel GPU", "name", name)
		gm.GpuDataMap[id] = &system.GPUData{Name: name}
	}
	gpu := gm.GpuDataMap[id]
	
	// Calculate total GPU usage from all engines
	totalUsage := intelGpuInfo.Engines.Render3D.Busy +
		intelGpuInfo.Engines.Blitter.Busy +
		intelGpuInfo.Engines.Video.Busy +
		intelGpuInfo.Engines.VideoEnhance.Busy
	
	// Use the higher of GPU or Package power
	power := intelGpuInfo.Power.GPU
	if intelGpuInfo.Power.Package > power {
		power = intelGpuInfo.Power.Package
	}

	// Update GPU data
	gpu.Usage += totalUsage // Will be averaged by Count in GetCurrentData
	gpu.Power += power     // Will be averaged by Count in GetCurrentData
	gpu.Count++

	slog.Debug("Updated Intel GPU data", 
		"usage", totalUsage,
		"power", power,
		"count", gpu.Count)

	return true
}

// sums and resets the current GPU utilization data since the last update
func (gm *GPUManager) GetCurrentData() map[string]system.GPUData {
	gm.Lock()
	defer gm.Unlock()

	// check for GPUs with the same name
	nameCounts := make(map[string]int)
	for _, gpu := range gm.GpuDataMap {
		nameCounts[gpu.Name]++
	}

	// copy / reset the data
	gpuData := make(map[string]system.GPUData, len(gm.GpuDataMap))
	for id, gpu := range gm.GpuDataMap {
		// sum the data
		gpu.Temperature = twoDecimals(gpu.Temperature)
		gpu.MemoryUsed = twoDecimals(gpu.MemoryUsed)
		gpu.MemoryTotal = twoDecimals(gpu.MemoryTotal)
		gpu.Usage = twoDecimals(gpu.Usage / gpu.Count)
		gpu.Power = twoDecimals(gpu.Power / gpu.Count)
		// reset the count
		gpu.Count = 1
		// dereference to avoid overwriting anything else
		gpuCopy := *gpu
		// append id to the name if there are multiple GPUs with the same name
		if nameCounts[gpu.Name] > 1 {
			gpuCopy.Name = fmt.Sprintf("%s %s", gpu.Name, id)
		}
		gpuData[id] = gpuCopy
	}
	slog.Debug("GPU", "data", gpuData)
	return gpuData
}

// detectGPUs checks for the presence of GPU management tools (nvidia-smi, rocm-smi, tegrastats)
// in the system path. It sets the corresponding flags in the GPUManager struct if any of these
// tools are found. If none of the tools are found, it returns an error indicating that no GPU
// management tools are available.
func (gm *GPUManager) detectGPUs() error {
	if _, err := exec.LookPath(nvidiaSmiCmd); err == nil {
		gm.nvidiaSmi = true
	}
	if _, err := exec.LookPath(rocmSmiCmd); err == nil {
		gm.rocmSmi = true
	}
	if _, err := exec.LookPath(tegraStatsCmd); err == nil {
		gm.tegrastats = true
	}
	if _, err := exec.LookPath(intelGpuTopCmd); err == nil {
		gm.intelGpuTop = true
	}
	if gm.nvidiaSmi || gm.rocmSmi || gm.tegrastats || gm.intelGpuTop {
		return nil
	}
	return fmt.Errorf("no GPU found - install nvidia-smi, rocm-smi, tegrastats, or intel-gpu-tools")
}

// startCollector starts the appropriate GPU data collector based on the command
func (gm *GPUManager) startCollector(command string) {
	collector := gpuCollector{
		name: command,
	}
	switch command {
	case nvidiaSmiCmd:
		collector.cmdArgs = []string{"-l", nvidiaSmiInterval,
			"--query-gpu=index,name,temperature.gpu,memory.used,memory.total,utilization.gpu,power.draw",
			"--format=csv,noheader,nounits"}
		collector.parse = gm.parseNvidiaData
		go collector.start()
	case tegraStatsCmd:
		collector.cmdArgs = []string{"--interval", tegraStatsInterval}
		collector.parse = gm.getJetsonParser()
		go collector.start()
	case rocmSmiCmd:
		collector.cmdArgs = []string{"--showid", "--showtemp", "--showuse", "--showpower", "--showproductname", "--showmeminfo", "vram", "--json"}
		collector.parse = gm.parseAmdData
		go func() {
			failures := 0
			for {
				if err := collector.collect(); err != nil {
					failures++
					if failures > maxFailureRetries {
						break
					}
					slog.Warn("Error collecting AMD GPU data", "err", err)
				}
				time.Sleep(rocmSmiInterval)
			}
		}()
	case intelGpuTopCmd:
		collector.cmdArgs = []string{
			"-J",           // JSON output
			"-s", "1",      // 1 second refresh rate
			"-o", "-",      // Output to stdout
		}
		collector.parse = gm.parseIntelGpuTopData
		go func() {
			failures := 0
			for {
				if err := collector.collect(); err != nil {
					failures++
					if failures > maxFailureRetries {
						slog.Error("Intel GPU data collection failed too many times, stopping", "failures", failures, "err", err)
						break
					}
					slog.Warn("Error collecting Intel GPU data", "err", err, "failures", failures)
					time.Sleep(retryWaitTime)
					continue
				}
				failures = 0 // Reset failure count on successful collection
				time.Sleep(intelGpuTopInterval)
			}
		}()
	}
}

// NewGPUManager creates and initializes a new GPUManager
func NewGPUManager() (*GPUManager, error) {
	var gm GPUManager
	if err := gm.detectGPUs(); err != nil {
		return nil, err
	}
	gm.GpuDataMap = make(map[string]*system.GPUData)

	if gm.nvidiaSmi {
		gm.startCollector(nvidiaSmiCmd)
	}
	if gm.rocmSmi {
		gm.startCollector(rocmSmiCmd)
	}
	if gm.tegrastats {
		gm.startCollector(tegraStatsCmd)
	}
	if gm.intelGpuTop {
		gm.startCollector(intelGpuTopCmd)
	}

	return &gm, nil
}
