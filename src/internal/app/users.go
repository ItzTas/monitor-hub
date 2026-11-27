package app

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	terminalio "golang.org/x/term"

	"github.com/ItzTas/monitor-hub/src/internal/accounts"
	"github.com/ItzTas/monitor-hub/src/internal/consumers"
	"github.com/ItzTas/monitor-hub/src/internal/core"
)

var errUsersUsage = usageError{errors.New("usage: monitor-hub users create [--admin] <login> | passwd <login> | list | delete <login>")}

type userAction func(ctx context.Context, svc *accounts.Service, login string, term terminal) error

func usersCommand(ctx context.Context, args, environ []string, term terminal) error {
	if len(args) == 0 {
		return errUsersUsage
	}
	action, rest := args[0], args[1:]
	if action == "list" && len(rest) == 0 {
		return withAccounts(ctx, environ, func(svc *accounts.Service) error { return listUsers(ctx, svc, term) })
	}
	admin := action == "create" && len(rest) > 0 && rest[0] == "--admin"
	if admin {
		rest = rest[1:]
	}
	if len(rest) != 1 {
		return errUsersUsage
	}
	login := rest[0]
	named := map[string]userAction{
		"create": createUser(admin),
		"passwd": changePassword,
		"delete": deleteUser,
	}
	run, ok := named[action]
	if !ok {
		return errUsersUsage
	}
	if !consumers.ValidName(login) {
		return usageError{accounts.ErrInvalidLogin}
	}
	return withAccounts(ctx, environ, func(svc *accounts.Service) error { return run(ctx, svc, login, term) })
}

func withAccounts(ctx context.Context, environ []string, run func(*accounts.Service) error) error {
	databaseURL, err := core.LoadDatabaseURL(environ, os.ReadFile)
	if err != nil {
		return err
	}
	pool, err := core.OpenDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return run(accounts.NewService(pool, time.Now))
}

func createUser(admin bool) userAction {
	return func(ctx context.Context, svc *accounts.Service, login string, term terminal) error {
		password, err := readPassword(term)
		if err != nil {
			return err
		}
		if err := svc.CreateUser(ctx, login, password, admin); err != nil {
			return err
		}
		out := &printer{w: term.stdout}
		if admin {
			out.printf("person %s created as an admin\n", login)
			return out.err
		}
		out.printf("person %s created\n", login)
		return out.err
	}
}

func changePassword(ctx context.Context, svc *accounts.Service, login string, term terminal) error {
	password, err := readPassword(term)
	if err != nil {
		return err
	}
	if err := svc.SetPassword(ctx, login, password); err != nil {
		return err
	}
	out := &printer{w: term.stdout}
	out.printf("password of %s changed; their sessions ended\n", login)
	return out.err
}

func deleteUser(ctx context.Context, svc *accounts.Service, login string, term terminal) error {
	if err := svc.DeleteUser(ctx, login); err != nil {
		return err
	}
	out := &printer{w: term.stdout}
	out.printf("person %s deleted\n", login)
	return out.err
}

func listUsers(ctx context.Context, svc *accounts.Service, term terminal) error {
	users, err := svc.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		out := &printer{w: term.stdout}
		out.printf("no people\n")
		return out.err
	}
	yesNo := map[bool]string{true: "yes", false: "no"}
	table := tabwriter.NewWriter(term.stdout, 0, 0, 2, ' ', 0)
	out := &printer{w: table}
	out.printf("LOGIN\tADMIN\tDISABLED\tCREATED\n")
	for _, u := range users {
		out.printf("%s\t%s\t%s\t%s\n", u.Login, yesNo[u.Admin], yesNo[u.Disabled], formatTime(&u.CreatedAt))
	}
	if out.err != nil {
		return out.err
	}
	return table.Flush()
}

// readPassword asks twice without echo on a terminal and reads one line otherwise.
func readPassword(term terminal) (string, error) {
	if file, ok := term.stdin.(*os.File); ok && terminalio.IsTerminal(int(file.Fd())) {
		return promptPassword(int(file.Fd()), term.stderr)
	}
	line, err := bufio.NewReader(term.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func promptPassword(fd int, stderr io.Writer) (string, error) {
	var answers [2]string
	for i, prompt := range []string{"Password: ", "Repeat it: "} {
		out := &printer{w: stderr}
		out.printf("%s", prompt)
		if out.err != nil {
			return "", out.err
		}
		answer, err := terminalio.ReadPassword(fd)
		out.printf("\n")
		if err != nil {
			return "", err
		}
		answers[i] = string(answer)
	}
	if answers[0] != answers[1] {
		return "", errors.New("the two passwords differ")
	}
	return answers[0], nil
}
