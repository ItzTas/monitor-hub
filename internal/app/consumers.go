package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ItzTas/monitor-hub/internal/consumers"
	"github.com/ItzTas/monitor-hub/internal/core"
)

var errConsumersUsage = usageError{errors.New("usage: monitor-hub consumers add|rotate|revoke <name> | list")}

func consumersCommand(ctx context.Context, args, environ []string, term terminal) error {
	if len(args) == 0 {
		return errConsumersUsage
	}
	action, rest := args[0], args[1:]
	if action == "list" && len(rest) == 0 {
		return withConsumers(ctx, environ, func(svc *consumers.Service) error { return listConsumers(ctx, svc, term) })
	}
	if len(rest) != 1 {
		return errConsumersUsage
	}
	name := rest[0]
	named := map[string]func(ctx context.Context, svc *consumers.Service, name string, term terminal) error{
		"add":    addConsumer,
		"rotate": rotateConsumer,
		"revoke": revokeConsumer,
	}
	run, ok := named[action]
	if !ok {
		return errConsumersUsage
	}
	if !consumers.ValidName(name) {
		return errInvalidConsumerName
	}
	return withConsumers(ctx, environ, func(svc *consumers.Service) error {
		return consumerError(name, run(ctx, svc, name, term))
	})
}

var errInvalidConsumerName = usageError{errors.New("consumer names use lowercase letters, digits and single hyphens, 2 to 31 characters, starting with a letter")}

func withConsumers(ctx context.Context, environ []string, run func(*consumers.Service) error) error {
	databaseURL, err := core.LoadDatabaseURL(environ, os.ReadFile)
	if err != nil {
		return err
	}
	pool, err := core.OpenDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return run(consumers.NewService(pool))
}

func consumerError(name string, err error) error {
	if errors.Is(err, consumers.ErrInvalidName) {
		return errInvalidConsumerName
	}
	if errors.Is(err, consumers.ErrNotFound) {
		return fmt.Errorf("no consumer named %s", name)
	}
	if errors.Is(err, consumers.ErrExists) {
		return fmt.Errorf("a consumer named %s already exists", name)
	}
	return err
}

func addConsumer(ctx context.Context, svc *consumers.Service, name string, term terminal) error {
	token, err := svc.Add(ctx, name)
	if err != nil {
		return err
	}
	out := &printer{w: term.stdout}
	out.printf("consumer %s created; its token, shown only now:\n%s\n", name, token)
	return out.err
}

func rotateConsumer(ctx context.Context, svc *consumers.Service, name string, term terminal) error {
	token, err := svc.Rotate(ctx, name)
	if err != nil {
		return err
	}
	out := &printer{w: term.stdout}
	out.printf("consumer %s has a new token, shown only now:\n%s\n", name, token)
	return out.err
}

func revokeConsumer(ctx context.Context, svc *consumers.Service, name string, term terminal) error {
	if err := svc.Revoke(ctx, name); err != nil {
		return err
	}
	out := &printer{w: term.stdout}
	out.printf("consumer %s revoked\n", name)
	return out.err
}

func listConsumers(ctx context.Context, svc *consumers.Service, term terminal) error {
	all, err := svc.List(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		out := &printer{w: term.stdout}
		out.printf("no consumers\n")
		return out.err
	}
	table := tabwriter.NewWriter(term.stdout, 0, 0, 2, ' ', 0)
	out := &printer{w: table}
	out.printf("NAME\tPOSITION\tBEHIND\tLAST ACK\tLAST SEEN\n")
	for _, c := range all {
		out.printf("%s\t%d\t%d\t%s\t%s\n", c.Name, c.Position, c.Behind, formatTime(c.AckedAt), formatTime(c.SeenAt))
	}
	if out.err != nil {
		return out.err
	}
	return table.Flush()
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
