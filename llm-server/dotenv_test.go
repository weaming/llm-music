package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		key   string
		value string
		ok    bool
	}{
		{name: "普通", line: "OPENAI_MODEL=deepseek-flash", key: "OPENAI_MODEL", value: "deepseek-flash", ok: true},
		{name: "两侧空格", line: "  OPENAI_MODEL = deepseek-flash  ", key: "OPENAI_MODEL", value: "deepseek-flash", ok: true},
		{name: "export 前缀", line: "export OPENAI_MODEL=deepseek-flash", key: "OPENAI_MODEL", value: "deepseek-flash", ok: true},
		{name: "双引号", line: `OPENAI_MODEL="deepseek flash"`, key: "OPENAI_MODEL", value: "deepseek flash", ok: true},
		{name: "单引号", line: `OPENAI_MODEL='# 不是注释'`, key: "OPENAI_MODEL", value: "# 不是注释", ok: true},
		{name: "值里有等号", line: "TAP_URL=http://127.0.0.1:3050/v1/tap?a=1", key: "TAP_URL", value: "http://127.0.0.1:3050/v1/tap?a=1", ok: true},
		{name: "空值", line: "OPENAI_PROXY=", key: "OPENAI_PROXY", value: "", ok: true},
		{name: "注释", line: "# OPENAI_MODEL=deepseek-flash", ok: false},
		{name: "空行", line: "   ", ok: false},
		{name: "缺少等号", line: "https://api.deepseek.com/v1", ok: false},
		{name: "非法变量名", line: "OPENAI-MODEL=x", ok: false},
		{name: "数字开头", line: "1MODEL=x", ok: false},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			key, value, ok := parseEnvLine(item.line)
			if ok != item.ok {
				t.Fatalf("ok = %t, want %t", ok, item.ok)
			}
			if !item.ok {
				return
			}
			if key != item.key || value != item.value {
				t.Fatalf("得到 %q=%q, want %q=%q", key, value, item.key, item.value)
			}
		})
	}
}

// TestLoadDotEnvDoesNotOverride 守住"已存在的环境变量优先"：
// 否则外部注入的配置会被仓库里的文件悄悄改掉。
func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	path := writeDotEnv(t, "OPENAI_MODEL=from-file\nLLM_USAGE_LOG_PATH=/tmp/from-file.jsonl\n")

	t.Setenv("OPENAI_MODEL", "from-env")
	// 文件会新设这个键，测试结束后清掉，免得泄漏给同包的其他测试
	stashEnvVar(t, "LLM_USAGE_LOG_PATH")
	loadDotEnv(path)

	if got := os.Getenv("OPENAI_MODEL"); got != "from-env" {
		t.Fatalf("OPENAI_MODEL = %q, want from-env", got)
	}
	if got := os.Getenv("LLM_USAGE_LOG_PATH"); got != "/tmp/from-file.jsonl" {
		t.Fatalf("LLM_USAGE_LOG_PATH = %q, want /tmp/from-file.jsonl", got)
	}
}

// TestLoadDotEnvMissingFile 文件不存在时不该出错——除本地直跑外，部署时通常没有它。
func TestLoadDotEnvMissingFile(t *testing.T) {
	loadDotEnv(filepath.Join(t.TempDir(), "not-exists.env"))
}

func writeDotEnv(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 .env 失败: %v", err)
	}
	return path
}

// stashEnvVar 记住当前值，测试结束时恢复（含"原本不存在"的情形）。
func stashEnvVar(t *testing.T, name string) {
	t.Helper()

	value, existed := os.LookupEnv(name)
	t.Cleanup(func() {
		if existed {
			os.Setenv(name, value)
			return
		}
		os.Unsetenv(name)
	})
}
