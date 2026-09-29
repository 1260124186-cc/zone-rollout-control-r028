package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"solo-0014-zone-rollout-control/internal/allocation"
	"solo-0014-zone-rollout-control/internal/approval"
	"solo-0014-zone-rollout-control/internal/assessment"
	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/fleet"
	"solo-0014-zone-rollout-control/internal/httpapi"
	"solo-0014-zone-rollout-control/internal/plan"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/probe"
	"solo-0014-zone-rollout-control/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var (
		address          string
		snapshotPath     string
		shutdownWindow   time.Duration
		requireApproval  bool
		approvalRolesCSV string
		approvalTTL      time.Duration
	)
	flag.StringVar(&address, "addr", "127.0.0.1:8090", "HTTP listen address")
	flag.StringVar(&snapshotPath, "state", "data/zrc-state.json", "state snapshot path")
	flag.DurationVar(&shutdownWindow, "shutdown-timeout", 10*time.Second, "graceful shutdown window")
	flag.BoolVar(&requireApproval, "require-dual-approval", false, "require dual approval before activating new plans by default")
	flag.StringVar(&approvalRolesCSV, "approval-roles", "reviewer,approver", "comma-separated distinct roles required for dual approval")
	flag.DurationVar(&approvalTTL, "approval-ttl", 24*time.Hour, "how long a recorded approval stays valid")
	flag.Parse()
	if shutdownWindow <= 0 {
		return errors.New("shutdown-timeout must be positive")
	}
	if approvalTTL <= 0 {
		return errors.New("approval-ttl must be positive")
	}
	requiredRoles, err := parseApprovalRoles(approvalRolesCSV)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lock, err := platform.LockFile(snapshotPath + ".lock")
	if err != nil {
		return fmt.Errorf("lock snapshot: %w", err)
	}
	defer lock.Unlock()

	stateStore, err := store.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	clock := platform.SystemClock{}
	ids := platform.CryptoIDGenerator{}
	policy := plan.DualApprovalPolicy{
		DefaultRequired: requireApproval,
		RequiredRoles:   requiredRoles,
		TTL:             approvalTTL,
	}
	approvals := approval.New(stateStore, clock, ids, policy.DomainPolicy())
	plans := plan.New(stateStore, clock, ids, policy, approvals)
	nodes := fleet.New(stateStore, clock, ids)
	allocations := allocation.New(stateStore, clock, ids)
	probes := probe.New(stateStore, clock, ids, allocations)
	assessments := assessment.New(stateStore, clock, ids)
	api := httpapi.New(plans, nodes, allocations, probes, assessments, approvals, ids, logger)

	server := &http.Server{
		Addr:              address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	rootContext, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go expireFunction(rootContext, allocations, logger)

	serverResult := make(chan error, 1)
	go func() {
		logger.Info("server listening", "address", address, "state_path", snapshotPath)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverResult <- err
			return
		}
		serverResult <- nil
	}()

	select {
	case err := <-serverResult:
		if err != nil {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-rootContext.Done():
		logger.Info("shutdown requested")
	}

	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), shutdownWindow)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	if err := <-serverResult; err != nil {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	logger.Info("server stopped")
	return nil
}

func parseApprovalRoles(raw string) ([]domain.ApprovalRole, error) {
	parts := strings.Split(raw, ",")
	roles := make([]domain.ApprovalRole, 0, len(parts))
	seen := make(map[domain.ApprovalRole]struct{})
	for _, part := range parts {
		role := domain.ApprovalRole(strings.ToLower(strings.TrimSpace(part)))
		if role != domain.RoleReviewer && role != domain.RoleApprover {
			return nil, fmt.Errorf("approval role %q is not supported", part)
		}
		if _, duplicate := seen[role]; duplicate {
			return nil, fmt.Errorf("approval role %q is repeated", role)
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	if len(roles) < 2 {
		return nil, errors.New("dual approval requires at least two distinct roles")
	}
	return roles, nil
}

func expireFunction(ctx context.Context, service *allocation.Service, logger *slog.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := service.ExpireDue(ctx)
			if err != nil {
				logger.Error("lease expiry sweep failed", "error", err)
				continue
			}
			if count > 0 {
				logger.Info("expired leases", "count", count)
			}
		}
	}
}
