package common

import (
	"flag"
	"fmt"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"log"
	"os"
	"path/filepath"
	"testing"
)

var (
	Port         = flag.Int("port", 3000, "the listening port")
	PrintVersion = flag.Bool("version", false, "print version and exit")
	PrintHelp    = flag.Bool("help", false, "print help and exit")
	LogDir       = flag.String("log-dir", "./logs", "specify the log directory")
	// PreviewLevels 只读地打印等级重算的影响面后退出，不启动服务、不改任何数据。
	// 供运营在上线前评估「有多少历史用户会因等级补正而折扣变低」。
	PreviewLevels = flag.Bool("preview-levels", false,
		"print user level recalculation impact (read-only) and exit")
)

func printHelp() {
	fmt.Println("One API " + Version + " - All in one API service for OpenAI API.")
	fmt.Println("Copyright (C) 2023 JustSong. All rights reserved.")
	fmt.Println("GitHub: https://github.com/songquanpeng/one-api")
	fmt.Println("Usage: one-api [--port <port>] [--log-dir <log directory>] [--preview-levels] [--version] [--help]")
}

func init() {
	// 跳过测试二进制：go test 生成的二进制会在 testing.Main 里自行注册 -test.* 标志，
	// 那个时机晚于 init()。若在 init 里 flag.Parse() 会遇到未注册的 -test.* 而 os.Exit(2)，
	// 导致本项目所有依赖 common 包的单元测试无法运行。
	// testing.Testing() 是 Go 1.21+ 标准 API，只在 testing.Main 驱动的二进制里返回 true。
	if testing.Testing() {
		return
	}

	flag.Parse()

	if *PrintVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *PrintHelp {
		printHelp()
		os.Exit(0)
	}

	if os.Getenv("SESSION_SECRET") != "" {
		if os.Getenv("SESSION_SECRET") == "random_string" {
			logger.SysError("SESSION_SECRET is set to an example value, please change it to a random string.")
		} else {
			config.SessionSecret = os.Getenv("SESSION_SECRET")
		}
	}
	if os.Getenv("SQLITE_PATH") != "" {
		SQLitePath = os.Getenv("SQLITE_PATH")
	}
	if *LogDir != "" {
		var err error
		*LogDir, err = filepath.Abs(*LogDir)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := os.Stat(*LogDir); os.IsNotExist(err) {
			err = os.Mkdir(*LogDir, 0777)
			if err != nil {
				log.Fatal(err)
			}
		}
		logger.LogDir = *LogDir
	}
}
