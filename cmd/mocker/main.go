package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/xumaojie/mocker/internal/config"
	"github.com/xumaojie/mocker/internal/fs"
	"github.com/xumaojie/mocker/internal/server"
	"github.com/xumaojie/mocker/internal/service"
)

type rootSet []string

func (r *rootSet) String() string {
	return strings.Join(*r, ",")
}

func (r *rootSet) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine executable dir: %w", err)
	}
	execDir := filepath.Dir(execPath)

	sample := config.Config{
		Roots: []string{filepath.Join(execDir, "data")},
		HTTP: config.HTTPConfig{
			Address:  config.DefaultHTTPSAddress,
			CertFile: config.DefaultCertFile,
			KeyFile:  config.DefaultKeyFile,
		},
		System: config.SystemConfig{
			MemoryLimitMB: config.DefaultMemoryLimitMB,
		},
	}
	if err := os.MkdirAll(sample.Roots[0], 0o755); err != nil {
		return fmt.Errorf("prepare default root: %w", err)
	}

	if err := config.Save(path, &sample); err != nil {
		return err
	}
	log.Printf("默认配置 %s 已生成，将服务配置写入 %s 下的 services 数组", path, sample.Roots[0])
	return nil
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to configuration file")
	var additionalRoots rootSet
	flag.Var(&additionalRoots, "root", "allowed root directory (may be repeated)")
	httpAddr := flag.String("http", "", "override HTTPS listen address")
	flag.Parse()

	if err := ensureConfig(*cfgPath); err != nil {
		log.Fatalf("无法准备配置：%v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("unable to load config: %v", err)
	}
	if *httpAddr != "" {
		cfg.HTTP.Address = *httpAddr
	}

	combinedRoots := append([]string(nil), cfg.Roots...)
	combinedRoots = append(combinedRoots, additionalRoots...)
	if len(combinedRoots) == 0 {
		log.Fatal("at least one root directory must be configured")
	}

	fsMgr, err := fs.NewManager(combinedRoots)
	if err != nil {
		log.Fatalf("failed to prepare roots: %v", err)
	}

	svcMgr, err := service.NewManager(fsMgr, cfg)
	if err != nil {
		log.Fatalf("failed to initialize services: %v", err)
	}
	defer svcMgr.Stop()

	srv := server.NewServer(svcMgr, fsMgr, cfg.HTTP.Address, cfg, *cfgPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("listening on https://%s", cfg.HTTP.Address)
	if err := srv.Serve(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("https server failed: %v", err)
	}
}
