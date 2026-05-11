package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/models"
	"github.com/dujiao-next/internal/repository"

	"github.com/google/uuid"
)

func usage() {
	fmt.Fprintln(os.Stderr, `admin-tool: 后台管理员运维工具

用法:
  admin-tool list-admins                       列出所有管理员
  admin-tool reset-2fa --username <name>       重置指定管理员的 2FA

读取的配置文件与 server 相同（默认 config.yml）。`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]

	cfg := config.Load()
	if err := models.InitDB(cfg.Database.Driver, cfg.Database.DSN, models.DBPoolConfig{
		MaxOpenConns:           cfg.Database.Pool.MaxOpenConns,
		MaxIdleConns:           cfg.Database.Pool.MaxIdleConns,
		ConnMaxLifetimeSeconds: cfg.Database.Pool.ConnMaxLifetimeSeconds,
		ConnMaxIdleTimeSeconds: cfg.Database.Pool.ConnMaxIdleTimeSeconds,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "init db: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "list-admins":
		listAdmins()
	case "reset-2fa":
		username := parseFlag(os.Args[2:], "--username")
		if username == "" {
			fmt.Fprintln(os.Stderr, "missing --username")
			usage()
			os.Exit(1)
		}
		resetTOTP(username)
	default:
		usage()
		os.Exit(1)
	}
}

// parseFlag 简单解析 --flag value
func parseFlag(args []string, name string) string {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	v := fs.String(name[2:], "", "")
	_ = fs.Parse(args)
	return *v
}

func listAdmins() {
	repo := repository.NewAdminRepository(models.DB)
	admins, err := repo.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUSERNAME\tIS_SUPER\t2FA_ENABLED\tLAST_LOGIN")
	for _, a := range admins {
		enabled := "no"
		if a.TOTPEnabledAt != nil {
			enabled = "yes (" + a.TOTPEnabledAt.Format("2006-01-02") + ")"
		}
		last := "-"
		if a.LastLoginAt != nil {
			last = a.LastLoginAt.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(w, "%d\t%s\t%t\t%s\t%s\n", a.ID, a.Username, a.IsSuper, enabled, last)
	}
	_ = w.Flush()
}

func resetTOTP(username string) {
	repo := repository.NewAdminRepository(models.DB)
	logRepo := repository.NewAdminLoginLogRepository(models.DB)

	admin, err := repo.GetByUsername(username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookup: %v\n", err)
		os.Exit(1)
	}
	if admin == nil {
		fmt.Fprintf(os.Stderr, "no admin with username=%q\n", username)
		os.Exit(1)
	}
	if err := repo.ClearTOTP(admin.ID); err != nil {
		fmt.Fprintf(os.Stderr, "clear: %v\n", err)
		os.Exit(1)
	}
	rid := "cli-" + uuid.NewString()
	_ = logRepo.Create(&models.AdminLoginLog{
		AdminID:   admin.ID,
		Username:  admin.Username,
		EventType: constants.AdminLoginEvent2FAResetByAdmin,
		Status:    constants.AdminLoginStatusSuccess,
		ClientIP:  "cli",
		UserAgent: "admin-tool",
		RequestID: rid,
		// OperatorID: nil — CLI 操作没有操作者管理员
	})
	fmt.Printf("OK: 2FA reset for admin id=%d username=%s at %s\n", admin.ID, admin.Username, time.Now().Format(time.RFC3339))
}
