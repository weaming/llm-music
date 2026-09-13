# llm-music

把 LLM 的逐字输出流实时音乐化的实验：一个无损透传的 LLM 代理，加一个把旁路事件渲染成声音的播放器。

```
客户端 ──POST /v1/chat/completions──▶ llm-server ──▶ 上游 LLM
                                        │
                                  GET /v1/tap（SSE 旁路）
                                        ▼
                                   tap-player ──▶ 扬声器 / WAV
```

- **llm-server**：OpenAI Chat Completions 的无损透传代理。流式与非流式都原样转发（保留状态码与错误信封），自身只做三件事：补默认 model、记用量、发旁路。旁路逐帧发布上游 SSE 原文，**只推实时事件、不做历史补发**，无订阅者时零开销，且绝不阻塞主链路。
- **tap-player**：订阅旁路，把帧解析成增量（正文 / reasoning / 工具调用），映射成合成参数，按固定 16 分音符时钟渲染出实时音频。可同时录制 jsonl 素材，之后离线重放或渲染成 WAV。

## 一、安装

需要 Go 1.26+，两个程序都是纯 Go（无需 cgo）。

```bash
git clone git@github.com:weaming/llm-music.git
cd llm-music

cd llm-server && CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" .
cd ../tap-player && CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" .
```

装到 `$GOBIN`（默认 `~/go/bin`，确认它在 `PATH` 里），产物各约 6MB。

声音输出走 `ebitengine/oto`：macOS 开箱可用，Linux 需要 `libasound2`。没有音频设备也能用 `replay -no-audio` 离线渲染成 WAV。

## 二、编写 env

```bash
cp env.example .env
```

填 `OPENAI_API_KEY`、`OPENAI_BASE_URL`、`OPENAI_MODEL` 三项即可，端口默认 3050 与 tap-player 的默认 tap 地址对齐。

`llm-server` 启动时读**当前目录**的 `.env`（在仓库根目录跑即可）。已存在的环境变量优先，所以临时改一项不必动文件：`LLM_SERVER_PORT=3051 llm-server`。

`tap-player` 不读 `.env`，它的选项都有默认值，用命令行参数调（`-h` 看全部）。

## 三、运行

```bash
# 终端 1：代理服务
llm-server

# 终端 2：连上旁路，实时播放
tap-player play
```

之后任何打到 `:3050` 的流式请求都会变成声音，非流式请求只记用量、不发旁路。

如果 3050 已被别的进程占用（例如已部署的实例），换个端口并显式指给播放器：

```bash
LLM_SERVER_PORT=3051 llm-server
tap-player play -tap-url http://127.0.0.1:3051/v1/tap
```

录制素材与离线调试：

```bash
# 边播边录成 jsonl
tap-player play -record tmp/tap.jsonl

# 重放素材，倍速压缩空闲
tap-player replay -speed 2 tmp/tap.jsonl

# 无音频设备：渲染成 WAV
tap-player replay -no-audio -out tmp/tap.wav tmp/tap.jsonl
```

注意 `replay` 的文件参数要放在所有选项**之后**（Go flag 遇到位置参数就停止解析），命令行选项优先于环境变量。

## 端点

| 端点                        | 说明                                                             |
| --------------------------- | ---------------------------------------------------------------- |
| `POST /v1/chat/completions` | OpenAI 兼容透传，支持 `stream`；原样转发 body 与响应             |
| `GET /v1/tap`               | SSE 旁路，事件 `stream_start` / `frame` / `stream_end` / `error` |
| `GET /health`               | 存活检查                                                         |

所有请求可带 `X-LLM-Caller` 头，用于用量日志区分调用方（默认 `unknown`）。

## 音乐映射

- **时钟与事件解耦**：节奏位置由引擎自己的时钟决定，数据只影响密度 —— LLM token 的突发性进不了音频时序。
- **相位**：reasoning 是低音区长音（三角波样式，暗）；正文是中音区旋律（近正弦，中性）；代码是断奏（方波样式，亮）。代码围栏由 markdown ``` 判定，相位切换带迟滞防抖。
- **工具调用**：走独立打击通道，吸附到拍而非 16 分音符（底鼓每拍一下），带音高下滑给出 punch。
- **生命周期**：流正常结束奏上行终止式；上游报错奏下行错误动机，落点刻意避开主音，留"没说完"的感觉。
- **和声**：全部音高取自大调五声音阶，任意取音都协和。
- **混音**：鼓与旋律走两条混响总线（短反射 / 长混响），鼓点出现时侧链压低旋律。
- **静默**：连续 8 拍没有输入就彻底安静，长时间挂着不会变成噪音。
