// Linux 服务器基础硬件巡检工具。
//
// 编译：go build -o server_inventory server_inventory.go
// 运行：sudo ./server_inventory
// 输出：<服务器序列号>_<时间戳>.txt
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	line    = "----------------------------------------------------------------------------------------------------------------------------------"
	version = "v3.1"
)

type result struct {
	out  string
	err  string
	code int
}

type disk struct {
	name, kind, transport, size, model, serial, firmware, hours string
	rotational                                                  bool
}

type app struct {
	file        *os.File
	timeout     time.Duration
	serial      string
	cpuModel    string
	physicalCPU int
	memoryBytes uint64
	disks       []disk
	nics        []string
	gpus        []string
	sectionNo   int
	screen      bool
}

func main() {
	outputDir := flag.String("output", ".", "检测结果输出目录")
	serial := flag.String("serial", "", "手动指定服务器序列号")
	timeout := flag.Duration("timeout", 30*time.Second, "单条命令超时时间")
	cudaTests := flag.Bool("cuda-tests", true, "运行 NVLink、CUDA bandwidth 和 P2P 测试；使用 -cuda-tests=false 可跳过带宽/P2P 压力测试")
	flag.Parse()

	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "本程序需要在 Linux 服务器运行，当前系统为 %s\n", runtime.GOOS)
		os.Exit(2)
	}
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fatal("创建输出目录失败", err)
	}

	a := &app{timeout: *timeout, screen: true}
	a.collectSummary()
	if clean(*serial) != "" {
		a.serial = clean(*serial)
	}
	if a.serial == "" {
		a.serial, _ = os.Hostname()
	}
	a.serial = safeName(a.serial)
	name := fmt.Sprintf("%s_%s.txt", a.serial, time.Now().Format("20060102_150405"))
	path := filepath.Join(*outputDir, name)
	var err error
	a.file, err = os.Create(path)
	if err != nil {
		fatal("创建结果文件失败", err)
	}
	defer a.file.Close()

	a.writeReport(*cudaTests)
	fmt.Printf("系统信息收集完成。日志已保存至 %s\n", path)
}

func (a *app) collectSummary() {
	a.serial = first(
		read("/sys/class/dmi/id/product_serial"),
		read("/sys/class/dmi/id/chassis_serial"),
		read("/sys/class/dmi/id/board_serial"),
	)
	lscpu := a.run("lscpu", "-J").out
	a.cpuModel = jsonLikeField(lscpu, "Model name")
	a.physicalCPU = atoi(jsonLikeField(lscpu, "Socket(s)"))
	if a.cpuModel == "" {
		a.cpuModel = cpuInfoModel(readRaw("/proc/cpuinfo"))
	}
	a.memoryBytes = memoryTotal(readRaw("/proc/meminfo"))
	a.disks = parseDisks(a.run("lsblk", "-dn", "-P", "-b", "-o", "NAME,TYPE,TRAN,ROTA,SIZE,MODEL,SERIAL,REV").out)
	for i := range a.disks {
		a.disks[i].hours = a.diskPowerOnHours(a.disks[i])
	}
	for _, row := range strings.Split(a.run("lspci", "-nn").out, "\n") {
		low := strings.ToLower(row)
		if strings.Contains(low, "ethernet controller") || strings.Contains(low, "network controller") || strings.Contains(low, "infiniband controller") {
			a.nics = append(a.nics, strings.TrimSpace(row))
		}
	}
	for _, row := range strings.Split(a.run("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").out, "\n") {
		if row = clean(row); row != "" {
			a.gpus = append(a.gpus, row)
		}
	}
}

func (a *app) writeReport(cudaTests bool) {
	a.header()
	a.printf("开始收集系统信息，序列号: %s\n", a.serial)
	a.section("Summary")
	nvme, sataSSD, sataHDD, sas := 0, 0, 0, 0
	for _, d := range a.disks {
		switch d.kind {
		case "NVMe SSD":
			nvme++
		case "SATA SSD":
			sataSSD++
		case "SATA HDD":
			sataHDD++
		case "SAS device":
			sas++
		}
	}
	gpuSummary := "未检测到 NVIDIA GPU，或 nvidia-smi 不可用"
	if len(a.gpus) == 0 {
	} else {
		counts := map[string]int{}
		for _, gpu := range a.gpus {
			counts[gpu]++
		}
		names := make([]string, 0, len(counts))
		for name := range counts {
			names = append(names, name)
		}
		sort.Strings(names)
		var parts []string
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%d x %s", counts[name], name))
		}
		gpuSummary = strings.Join(parts, ", ")
	}
	a.table([]string{"Item", "Value"}, [][]string{
		{"Serial Number", a.serial},
		{"CPU", fmt.Sprintf("%s (%d sockets)", show(a.cpuModel), a.physicalCPU)},
		{"Memory", humanBytes(a.memoryBytes)},
		{"Storage", fmt.Sprintf("NVMe SSD: %d, SATA SSD: %d, SATA HDD: %d, SAS: %d", nvme, sataSSD, sataHDD, sas)},
		{"Network Controllers", strconv.Itoa(len(a.nics))},
		{"GPU", gpuSummary},
	})

	a.divide()
	a.product()
	a.divide()
	a.cpu()
	a.divide()
	a.memory()
	a.divide()
	a.storage()
	a.divide()
	a.network()
	a.divide()
	a.gpu(cudaTests)
	a.divide()
	a.commandSection("FRU informations", "ipmitool", "fru", "print")
	a.divide()
	a.commandSection("SEL informations", "ipmitool", "sel", "elist")
	a.divide()
	a.dmesg()
	a.divide()
	a.commandSectionFileOnly("Lspci detailed informations", "lspci", "-vvvnn")
	a.divide()
	a.commandSectionFileOnly("NVIDIA SMI detailed informations", "nvidia-smi", "-q")
}

func (a *app) header() {
	left := []string{
		`    __  __           ______`,
		`    \ \/ /___ ______/ ____/`,
		`     \  / __ ` + "`" + `/ ___/ /`,
		`     / / /_/ / /  / /___`,
		`    /_/\__,_/_/   \____/`,
	}
	right := []string{
		"YarC get-server-info",
		"",
		"version : " + version,
		"By      : YarC",
		"",
	}
	const leftWidth = 36
	border := strings.Repeat("=", leftWidth+3+30)
	a.println(border)
	for i := range left {
		a.printf("%-*s | %s\n", leftWidth, left[i], right[i])
	}
	a.println(border)
	a.println("")
}

func (a *app) product() {
	a.section("Product informations")
	mc := a.run("ipmitool", "mc", "info")
	osInfo := osRelease(readRaw("/etc/os-release"))
	lan := a.run("ipmitool", "lan", "print", "1")
	a.table([]string{"Field", "Value"}, [][]string{
		{"Manufacturer", show(read("/sys/class/dmi/id/sys_vendor"))},
		{"Device Model", show(first(read("/sys/class/dmi/id/product_name"), read("/sys/class/dmi/id/product_version")))},
		{"Serial Number", a.serial},
		{"BIOS Firmware", show(read("/sys/class/dmi/id/bios_version"))},
		{"BMC Firmware", show(keyValue(mc.out, "Firmware Revision"))},
		{"OS Version", show(first(osInfo["PRETTY_NAME"], strings.TrimSpace(osInfo["NAME"]+" "+osInfo["VERSION_ID"])))},
		{"Kernel", show(clean(a.run("uname", "-srmo").out))},
		{"System Time", time.Now().Format(time.RFC3339)},
	})
	a.println("\nBMC Network Configuration (Channel 1)")
	a.table([]string{"IP", "Origin", "Subnet Mask", "Gateway", "MAC"}, [][]string{{
		show(keyValue(lan.out, "IP Address")),
		show(keyValue(lan.out, "IP Address Source")),
		show(keyValue(lan.out, "Subnet Mask")),
		show(keyValue(lan.out, "Default Gateway IP")),
		show(keyValue(lan.out, "MAC Address")),
	}})
	if mc.code != 0 || lan.code != 0 {
		a.println("提示: IPMI 信息通常需要 root 权限及可用的 ipmitool。")
	}
}

func (a *app) cpu() {
	a.section("CPU informations")
	lscpu := a.run("lscpu").out
	fields := colonFields(lscpu)
	sockets := uniqueDataLines(a.run("lscpu", "-p=SOCKET").out)
	if len(sockets) == 0 {
		for i := 0; i < a.physicalCPU; i++ {
			sockets = append(sockets, strconv.Itoa(i))
		}
	}
	manufacturer := "Unknown"
	if strings.Contains(strings.ToLower(a.cpuModel), "intel") {
		manufacturer = "Intel Corporation"
	}
	if strings.Contains(strings.ToLower(a.cpuModel), "amd") {
		manufacturer = "AMD"
	}
	var rows [][]string
	for _, socket := range sockets {
		rows = append(rows, []string{
			socket, manufacturer, a.cpuModel, fields["Core(s) per socket"], fields["Thread(s) per core"],
			mhzToGHz(fields["CPU min MHz"]), mhzToGHz(fields["CPU max MHz"]), "Enabled", "OK",
		})
	}
	a.table([]string{"Socket", "Manufacturer", "Model", "Cores", "Threads/Core", "Min GHz", "Max GHz", "State", "Health"}, rows)
}

func (a *app) memory() {
	a.section("Memory informations")
	dmi := a.run("dmidecode", "-t", "memory")
	rows := parseMemoryRows(dmi.out)
	a.table([]string{"id", "Name", "Cap", "Speed", "Type", "Manufacturer", "Part Number", "Serial"}, rows)
	a.printf("Total usable memory: %s\n", humanBytes(a.memoryBytes))
	if dmi.code != 0 {
		a.emitResult("dmidecode -t memory", dmi)
	}
}

func (a *app) storage() {
	a.section("Disk informations")
	var rows [][]string
	for i, d := range a.disks {
		size, _ := strconv.ParseUint(d.size, 10, 64)
		rows = append(rows, []string{strconv.Itoa(i + 1), d.kind, show(d.transport), d.name, d.model, humanBytes(size), d.serial, d.firmware, show(d.hours)})
	}
	a.table([]string{"id", "Type", "Transport", "Name", "Model", "Capacity", "Serial", "FW", "Power On Hours"}, rows)
}

func (a *app) diskPowerOnHours(d disk) string {
	device := "/dev/" + d.name
	smartOutput := a.run("smartctl", "-a", device).out
	for _, row := range strings.Split(smartOutput, "\n") {
		lower := strings.ToLower(row)
		// SATA/SAS SMART 属性表，例如：9 Power_On_Hours ... 2317
		if strings.Contains(lower, "power_on_hours") {
			fields := strings.Fields(row)
			if len(fields) > 0 {
				if hours := numbersOnly(fields[len(fields)-1]); hours != "" {
					return hours + " h"
				}
			}
		}
		// NVMe smartctl 输出，例如：Power On Hours: 1,234
		if strings.Contains(lower, "power on hours") {
			_, value, ok := strings.Cut(row, ":")
			if ok {
				if hours := numbersOnly(value); hours != "" {
					return hours + " h"
				}
			}
		}
		if strings.Contains(lower, "accumulated power on time") {
			_, value, ok := strings.Cut(row, ":")
			if ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}

	// 某些系统没有 smartctl，或控制器不透传 SMART；NVMe 再尝试 nvme-cli。
	if d.kind == "NVMe SSD" {
		out := a.run("nvme", "smart-log", device).out
		for _, key := range []string{"power_on_hours", "power on hours"} {
			if value := flexibleKeyValue(out, key); value != "" {
				if hours := numbersOnly(value); hours != "" {
					return hours + " h"
				}
			}
		}
	}
	return ""
}

func (a *app) network() {
	a.section("NIC informations")
	interfaces, _ := os.ReadDir("/sys/class/net")
	var rows [][]string
	for _, iface := range interfaces {
		if iface.Name() == "lo" {
			continue
		}
		base := filepath.Join("/sys/class/net", iface.Name())
		info := a.run("ethtool", "-i", iface.Name())
		bdf := pciBDF(base)
		short := shortBDF(bdf)
		capSpeed, capWidth, curSpeed, curWidth := a.pciLink(short)
		controller := strings.TrimSpace(a.run("lspci", "-s", short).out)
		if fields := strings.Fields(controller); len(fields) > 1 {
			controller = strings.TrimSpace(strings.TrimPrefix(controller, fields[0]))
		}
		rxErrors := readUint(filepath.Join(base, "statistics/rx_errors"))
		txErrors := readUint(filepath.Join(base, "statistics/tx_errors"))
		rows = append(rows, []string{
			show(short), capSpeed, capWidth, show(short), show(controller), curSpeed, curWidth,
			show(keyValue(info.out, "firmware-version")), iface.Name(), show(read(filepath.Join(base, "address"))),
			strconv.FormatUint(rxErrors+txErrors, 10), strconv.FormatUint(readUint(filepath.Join(base, "statistics/rx_dropped")), 10),
			strconv.FormatUint(readUint(filepath.Join(base, "statistics/tx_dropped")), 10),
		})
	}
	a.table([]string{"BUS_BDF", "Speed", "Width", "NIC_BDF", "Name", "Speed", "Width", "F/W-version", "Network Port", "MAC address", "Errors", "Dropped(-RX)", "Dropped(-TX)"}, rows)
}

func (a *app) gpu(cudaTests bool) {
	a.section("NVIDIA GPU card informations")
	gpuQuery := a.run("nvidia-smi", "--query-gpu=index,name,serial,pci.bus_id,vbios_version,temperature.gpu", "--format=csv,noheader,nounits")
	var gpuRows [][]string
	for _, row := range parseCSVRows(gpuQuery.out) {
		if len(row) >= 6 {
			gpuRows = append(gpuRows, []string{row[0], row[1], show(row[2]), row[3], row[4], row[5] + " C", "NVIDIA"})
		}
	}
	a.table([]string{"id", "Name", "Serial", "Location", "VBIOS/FW", "Temp", "Vendor"}, gpuRows)
	if gpuQuery.code != 0 {
		a.emitResult("nvidia-smi GPU table", gpuQuery)
	}
	a.section("NVIDIA GPU Serial Numbers")
	a.println("---------- nvidia-smi --query-gpu=index,name,serial ----------")
	a.emitCommand("nvidia-smi GPU serial", "nvidia-smi", "--query-gpu=index,name,serial", "--format=csv")
	a.section("NVIDIA GPU Errors (including ECC)")
	a.println("---------- nvidia-smi ECC error summary ----------")
	a.emitCommand("nvidia-smi ECC errors", "nvidia-smi", "--query-gpu=index,name,ecc.errors.corrected.aggregate.total,ecc.errors.uncorrected.aggregate.total", "--format=csv")
	a.println("\n---------- nvidia-smi -L ----------")
	a.emitCommand("nvidia-smi -L", "nvidia-smi", "-L")
	a.println("\n---------- nvidia-smi ----------")
	a.emitCommand("nvidia-smi", "nvidia-smi")
	a.section("NVIDIA NVLink Status")
	nvlink := a.run("nvidia-smi", "nvlink", "-s")
	if nvlink.code == 0 && clean(nvlink.out) == "" {
		a.println("当前 GPU 不支持 NVLink，或没有活动的 NVLink 链路。")
	} else {
		a.emitResult("nvidia-smi nvlink -s", nvlink)
	}

	a.section("CUDA Bandwidth Test")
	if !cudaTests {
		a.println("已通过 -cuda-tests=false 跳过。")
	} else {
		a.runCUDA("bandwidthTest", "--mode=quick")
	}
	a.section("CUDA P2P Bandwidth/Latency Test")
	if !cudaTests {
		a.println("已通过 -cuda-tests=false 跳过。")
	} else {
		a.runCUDA("p2pBandwidthLatencyTest")
	}
	a.section("NVIDIA GPU Formatted Information")
	a.formattedGPUInfo()
}

func (a *app) formattedGPUInfo() {
	query := a.run(
		"nvidia-smi",
		"--query-gpu=index,pci.bus_id,name,pcie.link.gen.current,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max",
		"--format=csv,noheader,nounits",
	)
	var rows [][]string
	for _, values := range parseCSVRows(query.out) {
		if len(values) < 7 {
			continue
		}
		fullBDF, name := normalizeFullBDF(values[1]), values[2]
		currentGen, maxGen := atoi(values[3]), atoi(values[4])
		currentWidth, maxWidth := atoi(values[5]), atoi(values[6])
		dmiSlot := a.dmiSlot(fullBDF)
		kernelSlot := read(filepath.Join("/sys/bus/pci/devices", strings.ToLower(fullBDF), "physical_slot"))
		rows = append(rows, []string{
			gpuBus(fullBDF), pcieSpeed(maxGen), widthText(maxWidth), gpuBus(fullBDF), name,
			show(first(kernelSlot, dmiSlot)), pcieSpeed(currentGen), pcieBandwidth(currentGen, currentWidth), show(dmiSlot),
		})
	}
	a.table([]string{"PCIe BDF", "PCIe Speed", "PCIe Width", "GPU BDF", "GPU Card Name", "Slot Number", "Speed", "Band Width", "Physical slot"}, rows)
	if query.code != 0 {
		a.emitResult("nvidia-smi formatted GPU information", query)
	}
}

func (a *app) dmiSlot(fullBDF string) string {
	out := a.run("dmidecode", "-t", "slot").out
	wanted := strings.ToLower(strings.TrimPrefix(fullBDF, "0000:"))
	for _, block := range strings.Split(out, "System Slot Information") {
		fields := colonFields(block)
		address := strings.ToLower(strings.TrimPrefix(fields["Bus Address"], "0000:"))
		if address == wanted {
			return first(fields["Designation"], fields["ID"])
		}
	}
	return ""
}

func (a *app) runCUDA(name string, args ...string) {
	paths := []string{}
	// 兼容旧版程序使用的 cuda-samples 源码树路径。
	if name == "bandwidthTest" {
		paths = append(paths, "/root/cuda-samples-master/Samples/1_Utilities/bandwidthTest/bandwidthTest")
	} else if name == "p2pBandwidthLatencyTest" {
		paths = append(paths, "/root/cuda-samples-master/Samples/5_Domain_Specific/p2pBandwidthLatencyTest/p2pBandwidthLatencyTest")
	}
	paths = append(paths,
		filepath.Join("/usr/local/cuda/extras/demo_suite", name),
		filepath.Join("/usr/local/cuda/samples/bin/x86_64/linux/release", name),
	)
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			a.emitResult(path, a.runWithTimeout(5*time.Minute, path, args...))
			return
		}
	}
	a.printf("警告: CUDA %s 未找到，跳过测试。\n", name)
}

func (a *app) dmesg() {
	a.section("Dmesg error informations")
	r := a.run("dmesg", "--level=err,warn")
	if r.code != 0 {
		r = a.run("dmesg")
		if r.code == 0 {
			var selected []string
			re := regexp.MustCompile(`(?i)error|fail|critical|fatal|mce|edac|aer|corrected|uncorrected`)
			for _, row := range strings.Split(r.out, "\n") {
				if re.MatchString(row) {
					selected = append(selected, row)
				}
			}
			r.out = strings.Join(selected, "\n")
		}
	}
	a.emitResult("dmesg errors", r)
}

func (a *app) commandSection(title, command string, args ...string) {
	a.section(title)
	a.emitCommand(strings.Join(append([]string{command}, args...), " "), command, args...)
}

func (a *app) commandSectionFileOnly(title, command string, args ...string) {
	a.section(title)
	result := a.run(command, args...)
	previous := a.screen
	a.screen = false
	a.emitResult(strings.Join(append([]string{command}, args...), " "), result)
	a.screen = previous
	if result.code == 0 {
		fmt.Printf("[内容较长，%s %s 的完整输出已仅写入 TXT 文件]\n", command, strings.Join(args, " "))
	} else {
		fmt.Printf("[命令执行失败] %s %s (exit=%d)\n", command, strings.Join(args, " "), result.code)
	}
}

func (a *app) emitCommand(label, command string, args ...string) {
	a.emitResult(label, a.run(command, args...))
}

func (a *app) emitResult(label string, r result) {
	if r.out != "" {
		a.println(strings.TrimRight(r.out, "\r\n"))
	}
	if r.code != 0 {
		a.printf("[命令执行失败] %s (exit=%d)\n", label, r.code)
		if clean(r.err) != "" {
			a.printf("%s\n", strings.TrimSpace(r.err))
		}
	}
}

func (a *app) run(command string, args ...string) result {
	return a.runWithTimeout(a.timeout, command, args...)
}

func (a *app) runWithTimeout(timeout time.Duration, command string, args ...string) result {
	if _, err := exec.LookPath(command); err != nil {
		return result{err: "command not found: " + command, code: 127}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		code = 124
		stderr.WriteString("\ncommand timed out")
	}
	return result{out: stripANSI(stdout.String()), err: stripANSI(stderr.String()), code: code}
}

func stripANSI(value string) string {
	// 部分版本的 nvidia-smi 即使输出到管道仍会带终端控制码。
	ansi := regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	return ansi.ReplaceAllString(value, "")
}

func (a *app) section(title string) {
	a.sectionNo++
	a.printf("\n[%d] %s\n", a.sectionNo, title)
}

func (a *app) divide() { a.println("") }

func (a *app) table(headers []string, rows [][]string) {
	if len(headers) == 0 {
		return
	}
	if len(rows) == 0 {
		row := make([]string, len(headers))
		row[0] = "N/A"
		rows = [][]string{row}
	}
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = visualWidth(header)
	}
	for _, row := range rows {
		for i := range headers {
			value := ""
			if i < len(row) {
				value = oneLine(row[i])
			}
			if width := visualWidth(value); width > widths[i] {
				widths[i] = width
			}
		}
	}
	for i := range widths {
		if widths[i] > 100 {
			widths[i] = 100
		}
	}
	border := func() {
		a.printf("+")
		for _, width := range widths {
			a.printf("%s+", strings.Repeat("-", width+2))
		}
		a.println("")
	}
	printRow := func(row []string) {
		a.printf("|")
		for i, width := range widths {
			value := ""
			if i < len(row) {
				value = fitCell(oneLine(row[i]), width)
			}
			a.printf(" %s |", padRight(value, width))
		}
		a.println("")
	}
	border()
	printRow(headers)
	border()
	for _, row := range rows {
		printRow(row)
	}
	border()
}

func (a *app) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.file, format, args...)
	if a.screen {
		_, _ = fmt.Printf(format, args...)
	}
}

func (a *app) println(text string) {
	_, _ = fmt.Fprintln(a.file, text)
	if a.screen {
		_, _ = fmt.Fprintln(os.Stdout, text)
	}
}

func parseMemoryRows(text string) [][]string {
	var rows [][]string
	for _, block := range strings.Split(text, "Memory Device") {
		values := colonFields(block)
		size := clean(values["Size"])
		if size == "" || strings.EqualFold(size, "No Module Installed") {
			continue
		}
		rows = append(rows, []string{
			strconv.Itoa(len(rows) + 1), show(first(values["Locator"], values["Bank Locator"])), size, first(values["Configured Memory Speed"], values["Configured Clock Speed"], values["Speed"]),
			values["Type"], clean(values["Manufacturer"]), clean(values["Part Number"]), clean(values["Serial Number"]),
		})
	}
	return rows
}

func colonFields(text string) map[string]string {
	values := map[string]string{}
	for _, row := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(row, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if ok && key != "" && values[key] == "" {
			values[key] = value
		}
	}
	return values
}

func uniqueDataLines(text string) []string {
	seen := map[string]bool{}
	var values []string
	for _, row := range strings.Split(text, "\n") {
		row = strings.TrimSpace(row)
		if row == "" || strings.HasPrefix(row, "#") || seen[row] {
			continue
		}
		seen[row] = true
		values = append(values, row)
	}
	sort.Strings(values)
	return values
}

func parseCSVRows(text string) [][]string {
	reader := csv.NewReader(strings.NewReader(text))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return nil
	}
	for _, row := range rows {
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
	}
	return rows
}

func pciBDF(netBase string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(netBase, "device"))
	if err != nil {
		return ""
	}
	matches := regexp.MustCompile(`(?i)[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]`).FindAllString(target, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func shortBDF(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) == 3 {
		return parts[1] + ":" + parts[2]
	}
	return value
}

func (a *app) pciLink(bdf string) (capSpeed, capWidth, curSpeed, curWidth string) {
	capSpeed, capWidth, curSpeed, curWidth = "N/A", "N/A", "N/A", "N/A"
	if bdf == "" {
		return
	}
	out := a.run("lspci", "-vv", "-s", bdf).out
	capRE := regexp.MustCompile(`LnkCap:.*Speed\s+([0-9.]+)GT/s.*Width\s+x([0-9]+)`)
	staRE := regexp.MustCompile(`LnkSta:.*Speed\s+([0-9.]+)GT/s.*Width\s+x([0-9]+)`)
	if match := capRE.FindStringSubmatch(out); len(match) == 3 {
		capSpeed, capWidth = match[1]+"GT/s", "x"+match[2]
	}
	if match := staRE.FindStringSubmatch(out); len(match) == 3 {
		curSpeed, curWidth = match[1]+"GT/s", "x"+match[2]
	}
	return
}

func netSpeed(netBase string) string {
	speed := read(filepath.Join(netBase, "speed"))
	if speed == "" || speed == "-1" {
		return "N/A"
	}
	return speed + " Mb/s"
}

func readUint(path string) uint64 {
	value, _ := strconv.ParseUint(read(path), 10, 64)
	return value
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func visualWidth(value string) int {
	width := 0
	for _, r := range value {
		if r >= 0x1100 {
			width += 2
		} else {
			width++
		}
	}
	return width
}

func fitCell(value string, width int) string {
	if visualWidth(value) <= width {
		return value
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	var b strings.Builder
	for _, r := range value {
		rw := 1
		if r >= 0x1100 {
			rw = 2
		}
		if visualWidth(b.String())+rw > width-3 {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "..."
}

func padRight(value string, width int) string {
	padding := width - visualWidth(value)
	if padding < 0 {
		padding = 0
	}
	return value + strings.Repeat(" ", padding)
}

func parseDisks(text string) []disk {
	re := regexp.MustCompile(`([A-Z]+)="((?:\\.|[^"])*)"`)
	var disks []disk
	for _, row := range strings.Split(text, "\n") {
		values := map[string]string{}
		for _, match := range re.FindAllStringSubmatch(row, -1) {
			values[match[1]] = strings.ReplaceAll(match[2], `\"`, `"`)
		}
		if values["TYPE"] != "disk" {
			continue
		}
		tran := strings.ToLower(values["TRAN"])
		name := values["NAME"]
		rotational := values["ROTA"] == "1" || strings.EqualFold(values["ROTA"], "true")
		kind := "Block device"
		switch {
		case tran == "nvme" || strings.HasPrefix(name, "nvme"):
			kind = "NVMe SSD"
		case tran == "sas":
			kind = "SAS device"
		case tran == "sata" && rotational:
			kind = "SATA HDD"
		case tran == "sata":
			kind = "SATA SSD"
		case tran == "" && strings.HasPrefix(name, "sd") && rotational:
			// RAID/HBA 后面的磁盘经常不提供 TRAN，使用盘名和旋转标志兜底。
			kind = "SATA HDD"
		case tran == "" && strings.HasPrefix(name, "sd"):
			kind = "SATA SSD"
		}
		disks = append(disks, disk{name: name, kind: kind, transport: tran, rotational: rotational, size: values["SIZE"], model: values["MODEL"], serial: values["SERIAL"], firmware: values["REV"]})
	}
	return disks
}

func jsonLikeField(text, wanted string) string {
	// lscpu -J 的不同版本都会包含 "field" 和 "data"；避免引入 JSON 数据输出，
	// 这里只对命令结果做轻量解析。
	fieldRE := regexp.MustCompile(`"field"\s*:\s*"` + regexp.QuoteMeta(wanted) + `:?"\s*,\s*"data"\s*:\s*"([^"]*)"`)
	if match := fieldRE.FindStringSubmatch(text); len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	for _, row := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(row, ":")
		if ok && strings.TrimSpace(key) == wanted {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func osRelease(text string) map[string]string {
	values := map[string]string{}
	for _, row := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(row, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	return values
}

func keyValue(text, wanted string) string {
	for _, row := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(row, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), wanted) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func flexibleKeyValue(text, wanted string) string {
	wanted = strings.ToLower(strings.ReplaceAll(wanted, "_", " "))
	for _, row := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(row, ":")
		key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "_", " "))
		if ok && key == wanted {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func numbersOnly(value string) string {
	match := regexp.MustCompile(`[0-9][0-9,]*`).FindString(value)
	return strings.ReplaceAll(match, ",", "")
}

func cpuInfoModel(text string) string {
	return keyValue(text, "model name")
}

func memoryTotal(text string) uint64 {
	re := regexp.MustCompile(`(?m)^MemTotal:\s+(\d+)\s+kB`)
	match := re.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0
	}
	value, _ := strconv.ParseUint(match[1], 10, 64)
	return value * 1024
}

func humanBytes(value uint64) string {
	if value == 0 {
		return "N/A"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	n, unit := float64(value), 0
	for n >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f%s", n, units[unit])
}

func mhzToGHz(value string) string {
	mhz, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || mhz <= 0 {
		return "N/A"
	}
	text := strconv.FormatFloat(mhz/1000, 'f', 3, 64)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	return text + " GHz"
}

func normalizeFullBDF(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Count(value, ":") == 1 {
		value = "0000:" + value
	}
	return value
}

func gpuBus(bdf string) string {
	parts := strings.Split(bdf, ":")
	if len(parts) >= 3 {
		return strings.ToUpper(parts[len(parts)-2])
	}
	return show(bdf)
}

func pcieSpeed(generation int) string {
	speeds := map[int]string{1: "2.5GT/s", 2: "5GT/s", 3: "8GT/s", 4: "16GT/s", 5: "32GT/s", 6: "64GT/s"}
	return show(speeds[generation])
}

func widthText(width int) string {
	if width <= 0 {
		return "N/A"
	}
	return "x" + strconv.Itoa(width)
}

func pcieBandwidth(generation, width int) string {
	// 单向有效带宽；Gen1/2 使用 8b/10b，Gen3-5 使用 128b/130b，Gen6 使用 FLIT 编码。
	perLane := map[int]float64{1: 0.25, 2: 0.50, 3: 0.9846, 4: 1.9692, 5: 3.9385, 6: 7.5625}
	value, ok := perLane[generation]
	if !ok || width <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.2f GB/s", value*float64(width))
}

func read(path string) string { return clean(readRaw(path)) }

func readRaw(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func first(values ...string) string {
	for _, value := range values {
		if value = clean(value); value != "" {
			return value
		}
	}
	return ""
}

func clean(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "\x00"))
	switch strings.ToLower(value) {
	case "", "none", "unknown", "n/a", "[n/a]", "null", "not specified", "to be filled by o.e.m.", "default string":
		return ""
	}
	return value
}

func show(value string) string {
	if value = clean(value); value == "" {
		return "N/A"
	}
	return value
}

func atoi(value string) int { n, _ := strconv.Atoi(strings.TrimSpace(value)); return n }

func safeName(value string) string {
	value = clean(value)
	if value == "" {
		return "UNKNOWN"
	}
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func fatal(message string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", message, err)
	os.Exit(1)
}
