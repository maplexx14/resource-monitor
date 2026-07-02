package main

import (
	"fmt"
	"log"
	"sort"
	"time"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

const historyLen = 300

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func gaugeColor(percent int) ui.Color {
	switch {
	case percent >= 85:
		return ui.ColorRed
	case percent >= 60:
		return ui.ColorYellow
	default:
		return ui.ColorGreen
	}
}

func netTotals() (sent, recv uint64) {
	counters, err := gnet.IOCounters(true)
	if err != nil {
		return 0, 0
	}
	for _, c := range counters {
		if c.Name == "lo" || c.Name == "lo0" {
			continue
		}
		sent += c.BytesSent
		recv += c.BytesRecv
	}
	return sent, recv
}

func topProcessRows(n int) [][]string {
	rows := [][]string{{"PID", "Имя", "CPU%", "MEM%"}}
	procs, err := process.Processes()
	if err != nil {
		return rows
	}
	type tp struct {
		pid  int32
		name string
		cpu  float64
		mem  float32
	}
	list := make([]tp, 0, len(procs))
	for _, p := range procs {
		cpuPct, err := p.CPUPercent()
		if err != nil {
			continue
		}
		name, err := p.Name()
		if err != nil || name == "" {
			continue
		}
		memPct, _ := p.MemoryPercent()
		list = append(list, tp{p.Pid, name, cpuPct, memPct})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].cpu > list[j].cpu })
	if len(list) > n {
		list = list[:n]
	}
	for _, p := range list {
		rows = append(rows, []string{
			fmt.Sprintf("%d", p.pid),
			p.name,
			fmt.Sprintf("%.1f", p.cpu),
			fmt.Sprintf("%.1f", p.mem),
		})
	}
	return rows
}

// pushCapped appends v and keeps the slice at most historyLen long.
func pushCapped(s []float64, v float64) []float64 {
	s = append(s, v)
	if len(s) > historyLen {
		s = s[len(s)-historyLen:]
	}
	return s
}

// tail returns up to the last n points, but never fewer than 2 (Plot requires it).
func tail(s []float64, n int) []float64 {
	if n < 2 {
		n = 2
	}
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func main() {
	if err := ui.Init(); err != nil {
		log.Fatalf("не удалось инициализировать termui: %v", err)
	}
	defer ui.Close()

	// Прогреваем счётчики CPU: дальше Percent(0, ...) возвращает дельту
	cpu.Percent(0, true)
	cpu.Percent(0, false)

	// --- Виджеты ---

	cpuPlot := widgets.NewPlot()
	cpuPlot.Title = " CPU, % (история) "
	cpuPlot.Data = [][]float64{{0, 0}}
	cpuPlot.MaxVal = 100
	cpuPlot.LineColors = []ui.Color{ui.ColorCyan}
	cpuPlot.AxesColor = ui.ColorWhite

	coreChart := widgets.NewBarChart()
	coreChart.Title = " Ядра CPU, % "
	coreChart.BarWidth = 5
	coreChart.MaxVal = 100
	coreChart.BarColors = []ui.Color{ui.ColorCyan}
	coreChart.NumStyles = []ui.Style{ui.NewStyle(ui.ColorWhite)}
	coreChart.LabelStyles = []ui.Style{ui.NewStyle(ui.ColorWhite)}
	coreChart.NumFormatter = func(v float64) string { return fmt.Sprintf("%.0f", v) }

	memGauge := widgets.NewGauge()
	memGauge.Title = " Память "

	swapGauge := widgets.NewGauge()
	swapGauge.Title = " Своп "

	diskGauge := widgets.NewGauge()
	diskGauge.Title = " Диск / "

	netPlot := widgets.NewPlot()
	netPlot.Title = " Сеть, КиБ/с "
	netPlot.Data = [][]float64{{0, 0}, {0, 0}}
	netPlot.LineColors = []ui.Color{ui.ColorGreen, ui.ColorMagenta}
	netPlot.AxesColor = ui.ColorWhite

	procTable := widgets.NewTable()
	procTable.Title = " Топ процессов по CPU "
	procTable.RowSeparator = false
	procTable.FillRow = true
	procTable.ColumnWidths = []int{8, 40, 8, 8}
	procTable.RowStyles[0] = ui.NewStyle(ui.ColorWhite, ui.ColorClear, ui.ModifierBold)
	procTable.Rows = topProcessRows(8)

	header := widgets.NewParagraph()
	header.Border = false

	grid := ui.NewGrid()
	termWidth, termHeight := ui.TerminalDimensions()
	grid.SetRect(0, 0, termWidth, termHeight)
	grid.Set(
		ui.NewRow(0.06, header),
		ui.NewRow(0.32,
			ui.NewCol(0.55, cpuPlot),
			ui.NewCol(0.45, coreChart),
		),
		ui.NewRow(0.10,
			ui.NewCol(1.0/3, memGauge),
			ui.NewCol(1.0/3, swapGauge),
			ui.NewCol(1.0/3, diskGauge),
		),
		ui.NewRow(0.26, netPlot),
		ui.NewRow(0.26, procTable),
	)

	// --- Состояние ---

	var cpuHistory, downHistory, upHistory []float64
	prevSent, prevRecv := netTotals()
	prevTime := time.Now()

	update := func() {
		now := time.Now()

		info, _ := host.Info()
		uptime := time.Duration(info.Uptime) * time.Second
		header.Text = fmt.Sprintf(" [Монитор ресурсов](mod:bold,fg:cyan) — %s | аптайм %s | %s | [q](mod:bold) — выход",
			info.Hostname, uptime.Round(time.Second), now.Format("15:04:05"))

		// CPU
		if total, err := cpu.Percent(0, false); err == nil && len(total) > 0 {
			cpuHistory = pushCapped(cpuHistory, total[0])
			cpuPlot.Title = fmt.Sprintf(" CPU %.1f%% (история) ", total[0])
		}
		if len(cpuHistory) >= 2 {
			cpuPlot.Data[0] = tail(cpuHistory, cpuPlot.Inner.Dx()-5)
		}
		if perCore, err := cpu.Percent(0, true); err == nil && len(perCore) > 0 {
			labels := make([]string, len(perCore))
			for i := range perCore {
				labels[i] = fmt.Sprintf("%d", i)
			}
			coreChart.Data = perCore
			coreChart.Labels = labels
		}

		// Память / своп / диск
		if vm, err := mem.VirtualMemory(); err == nil {
			memGauge.Percent = int(vm.UsedPercent)
			memGauge.Label = fmt.Sprintf("%d%% (%s / %s)", memGauge.Percent, humanBytes(vm.Used), humanBytes(vm.Total))
			memGauge.BarColor = gaugeColor(memGauge.Percent)
		}
		if sw, err := mem.SwapMemory(); err == nil && sw.Total > 0 {
			swapGauge.Percent = int(sw.UsedPercent)
			swapGauge.Label = fmt.Sprintf("%d%% (%s / %s)", swapGauge.Percent, humanBytes(sw.Used), humanBytes(sw.Total))
			swapGauge.BarColor = gaugeColor(swapGauge.Percent)
		}
		if du, err := disk.Usage("/"); err == nil {
			diskGauge.Percent = int(du.UsedPercent)
			diskGauge.Label = fmt.Sprintf("%d%% (%s / %s)", diskGauge.Percent, humanBytes(du.Used), humanBytes(du.Total))
			diskGauge.BarColor = gaugeColor(diskGauge.Percent)
		}

		// Сеть
		sent, recv := netTotals()
		secs := now.Sub(prevTime).Seconds()
		if secs > 0 {
			down := float64(recv-prevRecv) / secs
			up := float64(sent-prevSent) / secs
			downHistory = pushCapped(downHistory, down/1024)
			upHistory = pushCapped(upHistory, up/1024)
			netPlot.Title = fmt.Sprintf(" Сеть: ↓ %s/s  ↑ %s/s (зел. — приём, фиол. — отдача) ",
				humanBytes(uint64(down)), humanBytes(uint64(up)))
		}
		prevSent, prevRecv, prevTime = sent, recv, now
		if len(downHistory) >= 2 {
			w := netPlot.Inner.Dx() - 5
			netPlot.Data[0] = tail(downHistory, w)
			netPlot.Data[1] = tail(upHistory, w)
		}

		// Процессы
		procTable.Rows = topProcessRows(8)

		ui.Render(grid)
	}

	update()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	uiEvents := ui.PollEvents()

	for {
		select {
		case e := <-uiEvents:
			switch e.ID {
			case "q", "<C-c>":
				return
			case "<Resize>":
				payload := e.Payload.(ui.Resize)
				grid.SetRect(0, 0, payload.Width, payload.Height)
				ui.Clear()
				update()
			}
		case <-ticker.C:
			update()
		}
	}
}
