package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	collectInterval = 30 * time.Second
	maxAge          = 24 * time.Hour
	dataFile        = "data.json"
	listenAddr      = "127.0.0.1:5175"
)

type DataPoint struct {
	Timestamp int64   `json:"ts"`
	CPU       float64 `json:"cpu"`
	MemUsed   float64 `json:"mem_used"` // GB
	MemTotal  float64 `json:"mem_total"` // GB
	DiskUsed  float64 `json:"disk_used"` // GB
	DiskTotal float64 `json:"disk_total"` // GB
}

var (
	mu     sync.RWMutex
	points []DataPoint
	dataDir string
)

func main() {
	// Data dir next to binary
	exe, _ := os.Executable()
	dataDir = filepath.Dir(exe)
	if len(os.Args) > 1 {
		dataDir = os.Args[1]
	}

	loadData()
	go collectLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/api/data", serveData)

	log.Printf("sysmon listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

// --- Data Collection ---

func collectLoop() {
	// Collect immediately on start
	collect()
	ticker := time.NewTicker(collectInterval)
	for range ticker.C {
		collect()
	}
}

func collect() {
	p := DataPoint{
		Timestamp: time.Now().Unix(),
	}

	// CPU: compute from /proc/stat delta
	p.CPU = getCPU()

	// Memory: from /proc/meminfo
	p.MemUsed, p.MemTotal = getMemory()

	// Disk: root partition
	p.DiskUsed, p.DiskTotal = getDisk("/")

	mu.Lock()
	points = append(points, p)
	prune()
	saveDataLocked()
	mu.Unlock()
}

func prune() {
	cutoff := time.Now().Unix() - int64(maxAge.Seconds())
	i := 0
	for i < len(points) && points[i].Timestamp < cutoff {
		i++
	}
	if i > 0 {
		points = points[i:]
	}
}

// --- CPU from /proc/stat ---

var (
	prevIdle  uint64
	prevTotal uint64
	cpuOnce   bool
)

func getCPU() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return 0
			}
			var vals [10]uint64
			for i := 1; i < len(fields) && i <= 10; i++ {
				vals[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
			}
			idle := vals[3] + vals[4] // idle + iowait
			var total uint64
			for _, v := range vals {
				total += v
			}

			if !cpuOnce {
				prevIdle = idle
				prevTotal = total
				cpuOnce = true
				// Return current instant estimate
				runtime.Gosched()
				time.Sleep(100 * time.Millisecond)
				return getCPU()
			}

			dIdle := idle - prevIdle
			dTotal := total - prevTotal
			prevIdle = idle
			prevTotal = total

			if dTotal == 0 {
				return 0
			}
			return math.Round((1.0-float64(dIdle)/float64(dTotal))*1000) / 10
		}
	}
	return 0
}

// --- Memory from /proc/meminfo ---

func getMemory() (used, total float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvail uint64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvail = parseMemInfoValue(line)
		}
	}
	totalGB := float64(memTotal) / 1048576.0
	usedGB := float64(memTotal-memAvail) / 1048576.0
	return math.Round(usedGB*100) / 100, math.Round(totalGB*100) / 100
}

func parseMemInfoValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		return v // kB
	}
	return 0
}

// --- Disk via syscall ---

func getDisk(path string) (used, total float64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	usedBytes := totalBytes - freeBytes
	totalGB := float64(totalBytes) / 1073741824.0
	usedGB := float64(usedBytes) / 1073741824.0
	return math.Round(usedGB*100) / 100, math.Round(totalGB*100) / 100
}

// --- Persistence ---

func loadData() {
	f := filepath.Join(dataDir, dataFile)
	raw, err := os.ReadFile(f)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	json.Unmarshal(raw, &points)
	prune()
}

func saveDataLocked() {
	f := filepath.Join(dataDir, dataFile)
	raw, _ := json.Marshal(points)
	os.WriteFile(f, raw, 0644)
}

// --- HTTP Handlers ---

func serveData(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(points)
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

// --- Embedded HTML ---

var indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>sysmon</title>
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Crect width='64' height='64' rx='12' fill='%230d1117'/%3E%3Cpolyline points='4,36 18,36 24,16 30,48 36,28 42,36 60,36' fill='none' stroke='%2358a6ff' stroke-width='4' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    background: #0d1117;
    color: #c9d1d9;
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
    padding: 16px;
    min-height: 100vh;
  }
  h1 {
    font-size: 1.1rem;
    font-weight: 500;
    color: #58a6ff;
    margin-bottom: 4px;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .heartbeat {
    display: inline-block;
    width: 18px;
    height: 18px;
    animation: pulse 1.5s ease-in-out infinite;
  }
  .heartbeat svg { width: 100%; height: 100%; }
  @keyframes pulse {
    0%, 100% { transform: scale(1); opacity: 0.7; }
    15% { transform: scale(1.25); opacity: 1; }
    30% { transform: scale(1); opacity: 0.7; }
    45% { transform: scale(1.15); opacity: 0.95; }
    60% { transform: scale(1); opacity: 0.7; }
  }
  .subtitle {
    font-size: 0.75rem;
    color: #484f58;
    margin-bottom: 20px;
  }
  .grid {
    display: grid;
    grid-template-columns: 1fr;
    gap: 16px;
    max-width: 900px;
    margin: 0 auto;
  }
  .card {
    background: #161b22;
    border: 1px solid #21262d;
    border-radius: 8px;
    padding: 16px;
  }
  .card-header {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    margin-bottom: 12px;
  }
  .card-title {
    font-size: 1rem;
    font-weight: 500;
    color: #8b949e;
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }
  .card-value {
    font-size: 1.6rem;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  .cpu-color { color: #58a6ff; }
  .mem-color { color: #a371f7; }
  .disk-color { color: #3fb950; }
  canvas { width: 100% !important; height: 180px !important; }
  .range-btns {
    display: flex;
    gap: 6px;
    justify-content: center;
    margin-bottom: 16px;
  }
  .range-btns button {
    background: #21262d;
    border: 1px solid #30363d;
    color: #8b949e;
    padding: 8px 16px;
    border-radius: 4px;
    font-size: 0.9rem;
    cursor: pointer;
    transition: all 0.15s;
  }
  .range-btns button:hover { border-color: #58a6ff; color: #c9d1d9; }
  .range-btns button.active { background: #1f6feb; border-color: #1f6feb; color: #fff; }
  @media (min-width: 600px) {
    body { padding: 24px; }
    .grid { grid-template-columns: 1fr; gap: 20px; }
    canvas { height: 200px !important; }
  }
</style>
</head>
<body>
<div style="max-width:900px;margin:0 auto;">
  <h1><span class="heartbeat"><svg viewBox="0 0 24 24" fill="#58a6ff"><path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z"/></svg></span>sysmon</h1>
  <div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:4px;">
    <p class="subtitle" style="margin-bottom:0;">system monitor · 30s interval · 24h window</p>
    <p class="subtitle" style="margin-bottom:0;" id="last-updated"></p>
  </div>
  <div class="range-btns">
    <button onclick="setRange(1)" id="btn-1" class="active">1h</button>
    <button onclick="setRange(6)" id="btn-6">6h</button>
    <button onclick="setRange(12)" id="btn-12">12h</button>
    <button onclick="setRange(24)" id="btn-24">24h</button>
  </div>
  <div class="grid">
    <div class="card">
      <div class="card-header">
        <span class="card-title">CPU</span>
        <span class="card-value cpu-color" id="cpu-val">--%</span>
      </div>
      <canvas id="cpu-chart"></canvas>
    </div>
    <div class="card">
      <div class="card-header">
        <span class="card-title">Memory</span>
        <span class="card-value mem-color" id="mem-val">-- / -- GB</span>
      </div>
      <canvas id="mem-chart"></canvas>
    </div>
    <div class="card">
      <div class="card-header">
        <span class="card-title">Disk /</span>
        <span class="card-value disk-color" id="disk-val">-- / -- GB</span>
      </div>
      <canvas id="disk-chart"></canvas>
    </div>
  </div>
</div>

<script src="https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3/dist/chartjs-adapter-date-fns.bundle.min.js"></script>
<script>
const colors = { cpu: '#58a6ff', mem: '#a371f7', disk: '#3fb950' };
let rangeHours = 1;
let allData = [];

const chartOpts = (color, maxY) => ({
  responsive: true,
  maintainAspectRatio: false,
  animation: false,
  plugins: { legend: { display: false }, tooltip: { mode: 'index', intersect: false } },
  scales: {
    x: {
      type: 'time',
      time: { tooltipFormat: 'HH:mm:ss', displayFormats: { minute: 'HH:mm', hour: 'HH:mm' } },
      grid: { color: '#21262d' },
      ticks: { color: '#c9d1d9', font: { size: 14 }, maxTicksLimit: 6 }
    },
    y: {
      min: 0,
      max: maxY,
      grid: { color: '#21262d' },
      ticks: { color: '#c9d1d9', font: { size: 14 } }
    }
  },
  elements: { point: { radius: 0 }, line: { borderWidth: 1.5, tension: 0.2 } }
});

const dsOpts = (color) => ({
  borderColor: color,
  backgroundColor: color + '18',
  fill: true,
});

const cpuChart = new Chart(document.getElementById('cpu-chart'), {
  type: 'line',
  data: { datasets: [{ ...dsOpts(colors.cpu), data: [] }] },
  options: chartOpts(colors.cpu, 100)
});

const memChart = new Chart(document.getElementById('mem-chart'), {
  type: 'line',
  data: { datasets: [{ ...dsOpts(colors.mem), data: [] }] },
  options: chartOpts(colors.mem, 16)
});

const diskChart = new Chart(document.getElementById('disk-chart'), {
  type: 'line',
  data: { datasets: [{ ...dsOpts(colors.disk), data: [] }] },
  options: chartOpts(colors.disk, 100)
});

function setRange(h) {
  rangeHours = h;
  document.querySelectorAll('.range-btns button').forEach(b => b.classList.remove('active'));
  document.getElementById('btn-' + h).classList.add('active');
  render();
}

function render() {
  const cutoff = Date.now() - rangeHours * 3600 * 1000;
  const filtered = allData.filter(d => d.ts * 1000 >= cutoff);

  const cpuD = filtered.map(d => ({ x: d.ts * 1000, y: d.cpu }));
  const memD = filtered.map(d => ({ x: d.ts * 1000, y: d.mem_used }));
  const diskD = filtered.map(d => ({ x: d.ts * 1000, y: d.disk_used }));

  cpuChart.data.datasets[0].data = cpuD;
  memChart.data.datasets[0].data = memD;
  diskChart.data.datasets[0].data = diskD;

  // Update disk max Y
  if (filtered.length > 0) {
    const dt = filtered[0].disk_total;
    diskChart.options.scales.y.max = Math.ceil(dt / 10) * 10;
    memChart.options.scales.y.max = Math.ceil(filtered[0].mem_total);
  }

  cpuChart.update();
  memChart.update();
  diskChart.update();

  // Update current values and last updated time
  if (allData.length > 0) {
    const last = allData[allData.length - 1];
    document.getElementById('cpu-val').textContent = last.cpu.toFixed(1) + '%';
    document.getElementById('mem-val').textContent = last.mem_used.toFixed(1) + ' / ' + last.mem_total.toFixed(1) + ' GB';
    document.getElementById('disk-val').textContent = last.disk_used.toFixed(1) + ' / ' + last.disk_total.toFixed(1) + ' GB';
    const d = new Date(last.ts * 1000);
    const pad = n => String(n).padStart(2, '0');
    document.getElementById('last-updated').textContent = 'last updated: ' + d.getFullYear() + '-' + pad(d.getMonth()+1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }
}

async function fetchData() {
  try {
    const res = await fetch('/api/data');
    allData = await res.json();
    if (!allData) allData = [];
    render();
  } catch (e) {
    console.error('fetch error:', e);
  }
}

fetchData();
setInterval(fetchData, 30000);
</script>
</body>
</html>
`
