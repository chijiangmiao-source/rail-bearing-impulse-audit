# 轨旁拾音脉冲复核 API（trackside-pulse-api）

车辆段轨旁声学检测的**纯后端 HTTP API**：接收一段采样振幅，定位疑似轴承冲击脉冲，
输出可逐点复核的闭区间、峰值下标、峰值、等级与基线。全部计算在本服务内完成，
不依赖任何外部信号处理库，无固定响应、无占位实现，同一输入永远得到逐字段一致的结果。

- 语言/框架：Go 1.25、Gin、testify
- 部署：Docker + Docker Compose（多阶段构建，distroless 运行镜像）
- 验收：内置名为 `verify` 的一次性验收服务，43 个契约场景

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

## 输入约束

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `sample_rate` | JSON number | 有限浮点，闭区间 `[1000, 48000]` Hz |
| `amplitudes` | number 数组 | 长度闭区间 `[64, 20000]`，每个元素必须是有限浮点 |
| `excluded_ranges` | 对象数组，可选 | 每项 `{"start":int,"end":int}`；端点落在 `[0, len(amplitudes)-1]`、`start <= end`、按 `start` 升序、互不重叠（可相邻）；排除后剩余样本 `≥ 64`。未传、`null` 或 `[]` 均表示不屏蔽 |

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

另有 `GET /healthz` 存活探针。

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

# 一次性验收服务（等待 API 健康后运行 43 个场景，退出码 0/1）
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
cmd/verify/main.go       一次性验收服务（testify 断言，43 个场景）
internal/pulse/          检测算法：接缝屏蔽/基线/候选/合并/过滤/峰值/等级
internal/api/            Gin 路由、JSON 解码与字段/下标级校验
Dockerfile               golang:1.25 多阶段构建 → distroless 静态镜像
docker-compose.yml       api 服务 + verify 一次性验收服务
```
