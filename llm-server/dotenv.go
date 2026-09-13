package main

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// dotEnvName 是默认读取的配置文件，相对进程当前目录。
const dotEnvName = ".env"

// init 在包级变量之后、main 之前把 .env 读进进程环境。
// 进程只认环境变量，而本地直跑时把配置放在 .env 里最顺手。
func init() {
	loadDotEnv(dotEnvName)
}

// loadDotEnv 读取 path 里的 KEY=VALUE 并写入进程环境。
//
// 已存在的环境变量**优先**，不被文件覆盖：部署时环境由外部注入，
// 临时覆盖单个值（如 `LLM_SERVER_PORT=3051 llm-server`）也不必改文件。
// 文件不存在是常态，静默跳过。
func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	loaded := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			log.Printf("[env] 设置 %s 失败: %v", key, err)
			continue
		}
		loaded++
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[env] 读取 %s 失败: %v", path, err)
		return
	}
	if loaded > 0 {
		log.Printf("[env] 已从 %s 加载 %d 个变量", path, loaded)
	}
}

// parseEnvLine 解析一行。跳过空行与 # 注释行，支持 `export ` 前缀，
// 值两侧成对的引号会被去掉，值内部的 `=` 原样保留。
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

	sep := strings.IndexByte(line, '=')
	if sep <= 0 {
		return "", "", false
	}

	key = strings.TrimSpace(line[:sep])
	if !isEnvKey(key) {
		return "", "", false
	}
	return key, unquoteValue(strings.TrimSpace(line[sep+1:])), true
}

// isEnvKey 只接受 [A-Za-z_][A-Za-z0-9_]*。出现别的字符基本是文件写错了
// （比如漏了 `=` 而整行是个 URL），忽略比设进环境更安全。
func isEnvKey(key string) bool {
	for index, char := range key {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char == '_':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// unquoteValue 去掉成对的引号 —— 值里有空格或 # 时得靠引号表达。
func unquoteValue(value string) string {
	if len(value) < 2 {
		return value
	}

	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}
