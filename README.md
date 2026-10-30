# get-server-info

单文件 Go 语言 Linux 服务器硬件巡检工具。采集结果以格式化 ASCII 表格写入 TXT 文件，适用于普通服务器及 NVIDIA GPU 服务器。

当前版本：`v3.2`

## 版本说明

- `v3.2`：新增 IPMI 传感器信息采集，执行 `ipmitool sdr list`。
- `v3.1`：新增 YarC 作者信息栏；采集结果同步显示在屏幕并写入 TXT；FRU、SEL、dmesg、`lspci -vvvnn` 与 `nvidia-smi -q` 的完整内容仅写入 TXT。
- `v3.0`：单文件硬件巡检版本，包含格式化硬件信息、SMART 通电时长和默认 CUDA 测试。

## 采集内容

- 厂商、型号、序列号、BIOS、BMC、操作系统和内核
- BMC 网络配置及 FRU 信息
- CPU 型号、插槽、核心、线程、最低/最高频率
- 内存槽位、容量、速率、类型、厂商、料号和序列号
- NVMe、SATA SSD、SATA HDD、SAS 设备及硬盘通电时长
- 网卡 PCIe BDF、链路速率/宽度、固件、端口、MAC、错误和丢包计数
- NVIDIA GPU 序列号、温度、VBIOS、ECC、PCIe 链路和理论带宽
- NVLink 状态、CUDA bandwidthTest、CUDA p2pBandwidthLatencyTest
- IPMI SEL、传感器读数、dmesg 错误、完整 `lspci -vvvnn` 和 `nvidia-smi -q`

## 环境要求

- Linux amd64
- Go 1.18 或更高版本
- 建议使用 root 运行，以读取 DMI、SMART、IPMI 和内核日志

推荐安装以下系统工具：

```text
dmidecode  lscpu  lsblk  lspci  ethtool  ipmitool
nvidia-smi  smartctl  nvme
```

Ubuntu/Debian：

```bash
sudo apt update
sudo apt install -y dmidecode pciutils util-linux ethtool ipmitool smartmontools nvme-cli
```

Rocky Linux、AlmaLinux、RHEL：

```bash
sudo dnf install -y dmidecode pciutils util-linux ethtool ipmitool smartmontools nvme-cli
```

`nvidia-smi` 由 NVIDIA 驱动提供。

## 编译

```bash
git clone http://172.9.90.250:4080/yarc/get-server-info.git
cd get-server-info
go build -o server_inventory server_inventory.go
```

## 运行

```bash
sudo ./server_inventory
```

检测结果保存在当前目录，文件名格式为：

```text
服务器序列号_YYYYMMDD_HHMMSS.txt
```

除 FRU、SEL、dmesg、`lspci -vvvnn` 和 `nvidia-smi -q` 的明细外，所有采集结果也会在检测完成后同步显示到屏幕；这五项明细仍会完整保存在 TXT 报告中，终端仅显示完成或失败提示。

常用参数：

```bash
# 指定输出目录
sudo ./server_inventory -output /root/server-reports

# 手动指定服务器序列号
sudo ./server_inventory -serial SERVER-SN

# 修改普通命令的超时时间
sudo ./server_inventory -timeout 60s
```

## CUDA 测试

CUDA 测试默认启用，依次执行：

1. `nvidia-smi nvlink -s`
2. `bandwidthTest --mode=quick`
3. `p2pBandwidthLatencyTest`

程序会在以下位置查找 CUDA Sample：

```text
/root/cuda-samples-master/Samples/1_Utilities/bandwidthTest/bandwidthTest
/root/cuda-samples-master/Samples/5_Domain_Specific/p2pBandwidthLatencyTest/p2pBandwidthLatencyTest
/usr/local/cuda/extras/demo_suite/
/usr/local/cuda/samples/bin/x86_64/linux/release/
```

维护期间如需跳过带宽和 P2P 压力测试：

```bash
sudo ./server_inventory -cuda-tests=false
```

NVLink 状态仍会采集；不支持 NVLink 的 GPU 会在报告中明确标注。

## 硬盘通电时长

程序优先使用 `smartctl -a /dev/设备名` 读取 SATA、SAS 和 NVMe 硬盘的通电时长。NVMe 无法通过 `smartctl` 读取时，会回退到：

```bash
nvme smart-log /dev/nvmeXn1
```

硬件、控制器或工具无法提供该字段时，报告显示 `N/A`。

## 注意事项

- CUDA bandwidth 和 P2P 测试会短暂占用 GPU，请在允许产生测试负载时运行。
- 部分 RAID/HBA 控制器不透传 SMART，可能需要控制器专用参数或管理工具。
- 表格较宽，建议使用等宽字体并在宽终端或文本编辑器中查看 TXT 报告。
