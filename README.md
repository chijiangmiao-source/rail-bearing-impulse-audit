# 轨旁拾音脉冲复核 API（trackside-pulse-api）

车辆段轨旁声学检测的**纯后端 HTTP API**：接收一段采样振幅，定位疑似轴承冲击脉冲，
输出可逐点复核的闭区间、峰值下标、峰值、等级与基线。全部计算在本服务内完成，
不依赖任何外部信号处理库，无固定响应、无占位实现，同一输入永远得到逐字段一致的结果。

- 语言/框架：Go 1.25、Gin、testify
- 部署：Docker + Docker Compose（多阶段构建，distroless 运行镜像）
- 验收：内置名为 `verify` 的一次性验收服务，73 个契约场景

---

## 检测规则（与实现一一对应）

1. **基线**：参与判定样本绝对值的中位数；样本数为偶数时取中间两项的算术平均。
   传入 `excluded_ranges` 时，被排除样本既不进入基线，也不参与后续任何判定。
2. **阈值**：`6 × 基线`，只有 `|x|` **严格大于**阈值的样本才算超阈。
3. **候选区间**：连续超阈样本构成候选区间；排除样本强制断开候选。
4. **合并**：两个相邻候选之间未超阈样本数 **≤ 3** 时合并为一个闭区间；
   间隔 4 个及以上不合并。**排除区段是不可跨越的边界**：即使两侧超阈段
   相隔不超过 3 个样本，也分别合并和长度过滤。
5. **长度过滤**：按合并后闭区间包含的样本数（`end-start+1`）计长，**少于 4 个样本丢弃**。
6. **峰值**：区间内 `|x|` 的最大值；多个样本并列时**固定取最早下标**，峰值为非负绝对值。
7. **等级**：峰值 **严格大于 `12 × 基线`** 为 `severe`，否则为 `general`
   （恰好等于 12 倍判一般）。
8. **不可判定**：基线为零（`reason="baseline_zero"`）或过滤后没有任何保留区间
   （`reason="no_pulses"`）时返回 `decidable=false`、空脉冲列表，**绝不制造脉冲**。
9. 脉冲按 `start` 升序返回。整个服务无随机量、无 map 迭代序，输出确定。

## 车轮接缝屏蔽（`excluded_ranges`）

车辆段工程师可把检修记录中已确认的车轮接缝区段加入请求，标记不参与轴承
判断的样本：

- 每个元素为零基、**闭区间** `{"start": s, "end": e}`，端点必须是整数且落在
  `amplitudes` 的 `[0, len-1]` 内；
- 区间必须按 `start` **严格升序**、互不重叠（首尾相邻允许，如 `[0,2]` 与 `[3,5]`）；
- 屏蔽后至少保留 **64** 个有效样本，否则按 `min_length` 拒绝；
- 基线、阈值、等级均由**剩余样本**重新确定；
- 被排除样本既不能成为候选，也不能跨越合并；
- 响应在 `excluded_ranges` 中**原样回显实际采用的区间**（顺序、端点不变），
  便于逐点复核。未传字段或传 `[]`/`null` 时，请求与响应与旧版逐字段一致
  （响应中不出现该字段）。

## 可选脉冲度量（`include_metrics`）

车辆段工程师定位脉冲后还需比较冲击持续时间与区间整体振幅。单通道分析接受可选
开关 `include_metrics`：**只接受 JSON 布尔值**（`null` 与其他非布尔值一样按
`type` 错误定位拒绝）；未传或 `false` 时响应与旧版逐字段一致（不出现度量字段），
传 `true` 时每个已保留脉冲追加两个字段：

- `duration_ms`：冲击持续时间，按闭区间样本数（`end-start+1`）除以请求采样率
  再换算为毫秒；
- `rms_amplitude`：区间内**全部样本**的均方根振幅，采用缩放平方和算法
  （先除以区间最大幅值再平方求和），接近浮点上界的有限振幅也不会因中间平方
  溢出而产生无穷值。

基线为零或没有保留脉冲时仍返回原不可判定结果与空列表，**不伪造度量**。
该开关仅属于单通道接口：双通道关联入口不接受 `include_metrics`（在一侧出现
按未知字段拒绝），其左右分析结果也不出现度量字段，现有客户端无需调整。

## 输入约束

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `sample_rate` | JSON number | 有限浮点，闭区间 `[1000, 48000]` Hz |
| `amplitudes` | number 数组 | 长度闭区间 `[64, 20000]`，每个元素必须是有限浮点 |
| `excluded_ranges` | 对象数组，可选 | 每项 `{"start":int,"end":int}`；端点落在 `[0, len(amplitudes)-1]`、`start <= end`、按 `start` 升序、互不重叠（可相邻）；排除后剩余样本 `≥ 64`。未传、`null` 或 `[]` 均表示不屏蔽 |
| `include_metrics` | JSON boolean，可选 | 只接受 `true`/`false`；未传或 `false` 等价于关闭；`null` 与其他非布尔值均为 `type` 错误。开启后每个已保留脉冲追加 `duration_ms` 与 `rms_amplitude` |

非法请求返回 `400`，错误**定位到字段或样本下标**：

```json
{
  "error": "validation_failed",
  "field": "amplitudes",
  "index": 5,
  "constraint": "type",
  "message": "amplitudes[5] must be a finite JSON number"
}
```

`field` 为出错字段（字段级错误无 `index`），`index` 为从 0 开始的样本下标；
当 `field` 为 `excluded_ranges` 时，`index` 是出错**区间元素**的下标。
`constraint` 为 `required | type | range | finite | min_length | max_length |
order | overlap | unknown`：

- `range`：`start`/`end` 越出 `amplitudes`；
- `order`：区间倒置（`start > end`）或未按起点升序；
- `overlap`：与前一区间重叠；
- `min_length`：排除后有效样本少于 64；
- `type`/`required`/`unknown`：元素不是对象、端点不是整数、缺少端点或出现未知字段。

字段名**大小写敏感**：`Sample_rate`、`Include_Metrics`、`Tolerance_Samples` 等
大小写变体一律按 `unknown` 拒绝（双通道一侧内的变体以 `left.`/`right.` 前缀定位），
不会折叠到契约字段上执行分析或关联。

`NaN`/`Infinity`/`-Infinity` 不是合法 JSON 数值，服务同样定位到具体字段或样本下标。

## API

### `POST /api/v1/pulses/analyze`

请求：

```json
{ "sample_rate": 16000, "amplitudes": [2.0, 2.0, 13.0] }
```

需要屏蔽已确认接缝时追加可选字段：

```json
{
  "sample_rate": 16000,
  "amplitudes": [2.0, 30.0, 13.0],
  "excluded_ranges": [{ "start": 120, "end": 184 }]
}
```

成功（`200`）：

```json
{
  "sample_rate": 16000,
  "decidable": true,
  "baseline": 1,
  "pulses": [
    {
      "start": 5,
      "end": 15,
      "peak_index": 14,
      "peak": 20,
      "severity": "severe",
      "baseline": 1
    }
  ],
  "excluded_ranges": [{ "start": 120, "end": 184 }]
}
```

- `start`/`end`：零基、**闭区间**端点。
- `peak_index`：区间内最早的最大绝对值下标；`peak`：该绝对值。
- `severity`：`severe` 或 `general`。
- `baseline`：参与判定样本的绝对中位数（屏蔽时只算剩余样本），顶层与每个脉冲各回显一次，便于逐点复核。
- `excluded_ranges`：仅在请求显式给出非空数组时出现，原样回显实际采用的区间。
- 不可判定时：`decidable=false`、`reason` 为 `baseline_zero`/`no_pulses`、`pulses=[]`。

请求携带 `"include_metrics": true` 时，每个已保留脉冲追加 `duration_ms` 与
`rms_amplitude`：

```json
{
  "sample_rate": 16000,
  "decidable": true,
  "baseline": 2,
  "pulses": [
    {
      "start": 5,
      "end": 8,
      "peak_index": 6,
      "peak": 25,
      "severity": "severe",
      "baseline": 2,
      "duration_ms": 0.25,
      "rms_amplitude": 16.822603841260722
    }
  ]
}
```

另有 `GET /healthz` 存活探针。

## 双通道关联复核（`POST /api/v1/pulses/correlate`）

车辆段工程师把**同一轮对两侧轨道拾音器**的采样放在一次复核中。两侧各自携带
现有的 `sample_rate`、`amplitudes`、`excluded_ranges`，按单通道完全相同的
规则（含各自的采样率、振幅与接缝排除语义）独立完成脉冲分析，再建立一对一证据。

请求：

```json
{
  "tolerance_samples": 5,
  "left":  { "sample_rate": 16000, "amplitudes": [2.0, 13.0, 25.0] },
  "right": { "sample_rate": 16000, "amplitudes": [2.0, 13.0, 25.0] }
}
```

- `tolerance_samples`：**必填**，闭区间 `[0, 100]` 内的整数（0 合法，表示峰值
  下标必须完全相同）。
- `left`/`right`：**必填**的单通道对象，字段与约束同 `POST /pulses/analyze`，
  可各自携带 `excluded_ranges`。
- 两侧 `sample_rate` 必须**完全相等**（采样率不一致时以 `right.sample_rate`
  定位、`constraint: "sample_rate_mismatch"` 拒绝）。

成功（`200`）：

```json
{
  "sample_rate": 16000,
  "tolerance_samples": 5,
  "left":  { "sample_rate": 16000, "decidable": true, "baseline": 2, "pulses": [ ] },
  "right": { "sample_rate": 16000, "decidable": true, "baseline": 2, "pulses": [ ] },
  "pairs": [
    { "left_pulse_index": 0, "right_pulse_index": 0, "time_difference_samples": 0 }
  ],
  "left_unpaired": [],
  "right_unpaired": []
}
```

- `left`/`right`：两侧**原有分析结果原样保留**（字段形状与单通道接口逐字段一致，
  包括 `reason` 与 `excluded_ranges` 原样回显）。
- `pairs`：已配对证据。仅当**两侧均可判定**时才产生；候选按
  **（峰值时刻差、左侧峰值下标、右侧峰值下标）升序**依次选择，任一脉冲被占用后，
  涉及它的后续候选一律跳过。每个脉冲至多参与一对，脉冲数量不等或多个候选同时
  落入容差时仍得到唯一结果。证据按左侧峰值下标升序输出。
- `time_difference_samples`：两侧峰值下标的绝对差，恒满足
  `≤ tolerance_samples`。
- `left_unpaired`/`right_unpaired`：各侧未配对脉冲在该侧 `pulses` 列表中的
  下标，**固定按下标升序**；即使为空也序列化为 `[]`，绝不输出 `null`。
- 任一侧不可判定（`decidable=false`，如 `baseline_zero`/`no_pulses`）时不产生
  任何 `pairs`，可判定一侧的全部脉冲下标进入对应的 `unpaired` 列表。

错误（`400`）沿用现有错误结构，`field` 使用 `left`/`right`/`tolerance_samples`
前缀精确定位：

| 位置 | `field` | `constraint` |
| --- | --- | --- |
| 容差缺失 | `tolerance_samples` | `required` |
| 容差非整数/非数值/null（含 `NaN`/`Infinity`） | `tolerance_samples` | `type` |
| 容差超出 `[0,100]` | `tolerance_samples` | `range` |
| 一侧缺失或为 null/非标量对象 | `left` / `right` | `required` / `type` |
| 一侧内部字段错误 | `left.sample_rate`、`right.amplitudes` 等 | 单通道原有约束 |
| 一侧区间元素错误 | `left.excluded_ranges`（保留元素 `index`） | `order`/`range`/… |
| 一侧样本为 NaN/Infinity | `left.amplitudes`（保留样本 `index`） | `finite` |
| 两侧采样率不一致 | `right.sample_rate` | `sample_rate_mismatch` |

任何非法请求只返回错误信封，**不输出任何部分关联结果**。


## 本地运行（无 Docker）

需要 Go 1.25+。

```bash
make test       # 单元测试（算法 + HTTP 校验）
make run        # 本机 :8080 启动 API
make verify     # 对本机 API 跑一次性验收套件
```

也可：

```bash
go run ./cmd/api            # 默认 :8080，可用 PORT 覆盖
go run ./cmd/verify -base-url http://127.0.0.1:8080
```

## Docker Compose 运行

```bash
# 构建并后台启动 API；宿主端口可用 API_PORT 覆盖
API_PORT=9090 docker compose up --build -d

# 一次性验收服务（等待 API 健康后运行 73 个场景，退出码 0/1）
docker compose run --rm verify

# 或者构建后一起拉起，verify 跑完即退出
docker compose up --build
```

- `api` 服务：容器内监听 8080，宿主默认映射 8080，`API_PORT` 可覆盖。
- `verify` 服务：一次性（`restart: "no"`），通过 Compose 网络访问 `http://api:8080`，
  依赖 `api` 的容器健康状态（`api` 二进制内置 `-healthcheck` 探针）。

## 目录结构

```
cmd/api/main.go          HTTP 服务入口（含 -healthcheck 探针）
cmd/verify/main.go       一次性验收服务（testify 断言，69 个场景）
internal/pulse/          检测算法：单通道接缝屏蔽/基线/候选/合并/过滤/峰值/等级、可选脉冲度量；双通道配对
internal/api/            Gin 路由、JSON 解码与字段/下标级校验；双通道关联处理器与左右前缀定位
Dockerfile               golang:1.25 多阶段构建 → distroless 静态镜像
docker-compose.yml       api 服务 + verify 一次性验收服务
```
