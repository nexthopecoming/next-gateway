// Package command 为两个网关进程提供共享的 Cobra 命令、角色元数据和退出语义。
package command

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/nexthopecoming/next-gateway/internal/app"
	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
	"github.com/nexthopecoming/next-gateway/internal/core/version"
)

type roleSpec struct {
	use               string
	short             string
	defaultConfigPath string
}

// Main 执行指定角色的 CLI；命令失败时统一输出错误并以非零状态退出。
// 两个 main 只调用此函数，避免信号处理和 Cobra 配置随入口分叉。
func Main(r role.Role) {
	root, err := newRootCommand(r, config.DefaultManager())
	if err == nil {
		// SIGINT/SIGTERM 统一转为 ctx 取消，由应用层完成优雅退出；信号处理器不直接关闭资源。
		// 放在 ExecuteContext 之前，使配置加载等启动阶段也处于受控退出路径。
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = root.ExecuteContext(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// newRootCommand 创建根命令，并显式注入 serve 使用的启动配置 Manager。
func newRootCommand(r role.Role, manager *config.Manager) (*cobra.Command, error) {
	spec, err := describeRole(r)
	if err != nil {
		return nil, err
	}

	var configPath string
	root := &cobra.Command{
		Use:           spec.use,
		Short:         spec.short,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", spec.defaultConfigPath, "本地 YAML 配置路径")

	serve := &cobra.Command{
		Use:   "serve",
		Short: "启动网关 HTTP 服务",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// serve 只采用第一次启动加载结果，随后仍通过参数把配置显式交给 app。
			cfg, err := manager.Load(
				config.WithPath(configPath),
				config.WithFlags(cmd.Flags()),
				config.WithRole(r),
			)
			if err != nil {
				return err
			}
			return app.Run(cmd.Context(), cfg, r)
		},
	}
	config.BindFlags(serve.Flags())

	configCommand := &cobra.Command{Use: "config", Short: "配置工具"}
	validate := &cobra.Command{
		Use:   "validate",
		Short: "读取并校验本地配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// validate 每次独立读取，不能复用 serve 的成功或失败缓存。
			if _, err := config.Load(
				config.WithPath(configPath),
				config.WithFlags(cmd.Flags()),
				config.WithRole(r),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "配置有效: %s\n", config.ResolvePath(configPath))
			return nil
		},
	}
	config.BindFlags(validate.Flags())
	configCommand.AddCommand(validate)

	versionCommand := &cobra.Command{
		Use:   "version",
		Short: "显示构建版本",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprint(cmd.OutOrStdout(), version.Current().String())
		},
	}

	root.AddCommand(serve, configCommand, versionCommand)
	return root, nil
}

func describeRole(r role.Role) (roleSpec, error) {
	switch r {
	case role.Biz:
		return roleSpec{
			use:               r.String(),
			short:             "Next Gateway 业务网关",
			defaultConfigPath: "configs/biz-gateway.yaml",
		}, nil
	case role.AI:
		return roleSpec{
			use:               r.String(),
			short:             "Next Gateway AI 网关",
			defaultConfigPath: "configs/ai-gateway.yaml",
		}, nil
	default:
		return roleSpec{}, fmt.Errorf("未知网关角色 %q", r)
	}
}
