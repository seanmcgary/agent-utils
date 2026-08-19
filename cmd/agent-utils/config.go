package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// configCommand groups the machine-wide agent-utils settings: the webhook
// daemon's listen configuration and its HMAC secret.
//
// The word "config" already belongs to loop configuration in this repo
// (`--config`, docs/configuration.md, .agent-utils/configs/), so the Usage
// string says explicitly which "config" this is. Without that an operator
// who has only ever used `--config <loop file>` would reasonably guess this
// command edits a loop, not the machine.
func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "machine-wide settings; loop configuration lives in `.agent-utils/configs/`",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "print the machine-wide settings file",
				Flags: []cli.Flag{revealFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					s, err := settings.Load()
					if err != nil {
						return err
					}
					// No WithDefaults here: show must print what is really in the
					// file, not a value padded out with defaults it never saved.
					out, err := settings.Render(s, c.Bool("reveal"))
					if err != nil {
						return err
					}
					fmt.Print(out)
					return nil
				},
			},
			{
				Name:      "get",
				Usage:     "print one setting's value",
				Arguments: []cli.Argument{&cli.StringArg{Name: "key"}},
				Flags:     []cli.Flag{revealFlag()},
				Action: func(_ context.Context, c *cli.Command) error {
					key := c.StringArg("key")
					field, ok := settings.FieldFor(key)
					if !ok {
						return unknownKeyErr(key)
					}
					s, err := settings.Load()
					if err != nil {
						return err
					}
					val := field.Get(s)
					if field.Secret && !c.Bool("reveal") {
						val = settings.Redacted
					}
					fmt.Println(val)
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "set one setting; webhook.secret cannot be set this way, see `config webhook --rotate-secret`",
				Arguments: []cli.Argument{
					&cli.StringArg{Name: "key"},
					&cli.StringArg{Name: "value"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					s, err := settings.Load()
					if err != nil {
						return err
					}
					if err := setField(s, c.StringArg("key"), c.StringArg("value")); err != nil {
						return err
					}
					return settings.Save(s)
				},
			},
			{
				Name:      "unset",
				Usage:     "clear one setting back to its zero value",
				Arguments: []cli.Argument{&cli.StringArg{Name: "key"}},
				Action: func(_ context.Context, c *cli.Command) error {
					key := c.StringArg("key")
					field, ok := settings.FieldFor(key)
					if !ok {
						return unknownKeyErr(key)
					}
					s, err := settings.Load()
					if err != nil {
						return err
					}
					field.Unset(s)
					return settings.Save(s)
				},
			},
			configWebhookCommand(),
		},
	}
}

// revealFlag opts into printing a Field.Secret value in the clear. Without
// --reveal, get and show print settings.Redacted instead, so a terminal
// scrollback or a pasted screenshot does not casually leak the HMAC secret
// that authenticates every webhook delivery.
func revealFlag() *cli.BoolFlag {
	return &cli.BoolFlag{Name: "reveal", Usage: "show secret values instead of redacting them"}
}

// listenPortFlag and listenAddrFlag are declared once, here, and reused by
// E6's listener command. Two independent flag declarations for the same
// setting could drift to different names or defaults; a shared constructor
// makes that impossible by construction.
func listenPortFlag() *cli.IntFlag {
	return &cli.IntFlag{Name: "listen-port", Usage: "port the webhook daemon listens on"}
}

func listenAddrFlag() *cli.StringFlag {
	return &cli.StringFlag{Name: "listen-addr", Usage: "address the webhook daemon binds to"}
}

// unknownKeyErr lists every settable key, so a typo sends the operator to the
// right one instead of leaving them to guess or read source.
func unknownKeyErr(key string) error {
	fields := settings.Fields()
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.Key)
	}
	return fmt.Errorf("unknown key %q; known keys: %s", key, strings.Join(keys, ", "))
}

// setField applies value to key on s, going through settings.Fields so every
// parse and validation rule stays defined once, in internal/settings.
//
// Field.Set is nil for webhook.secret BY DESIGN (see settings.Fields): a
// hand-typed secret would be low entropy, possibly reused from elsewhere, and
// possibly logged in shell history. internal/settings' own tests call Set
// unguarded because they never exercise that field, so this nil check is the
// only thing standing between `config set webhook.secret x` and a panic.
func setField(s *settings.Settings, key, value string) error {
	field, ok := settings.FieldFor(key)
	if !ok {
		return unknownKeyErr(key)
	}
	if field.Set == nil {
		return fmt.Errorf("%s cannot be set directly; run `agent-utils config webhook --rotate-secret`", key)
	}
	return field.Set(s, value)
}

// configWebhookCommand applies every flag to one in-memory Settings and saves
// once, so a validation failure partway through (a bad --url, an
// out-of-range --listen-port) leaves the file untouched rather than half
// written.
func configWebhookCommand() *cli.Command {
	return &cli.Command{
		Name:  "webhook",
		Usage: "enable, disable or reconfigure the webhook daemon",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "enable", Usage: "turn the webhook daemon on"},
			&cli.BoolFlag{Name: "disable", Usage: "turn the webhook daemon off"},
			&cli.StringFlag{Name: "url", Usage: "public URL GitHub delivers webhooks to"},
			listenPortFlag(),
			listenAddrFlag(),
			&cli.BoolFlag{Name: "rotate-secret", Usage: "mint a new HMAC secret"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			enable, disable := c.Bool("enable"), c.Bool("disable")
			if enable && disable {
				return errors.New("--enable and --disable are alternatives; pass only one")
			}

			s, err := settings.Load()
			if err != nil {
				return err
			}

			if v := c.String("url"); v != "" {
				if err := setField(s, "webhook.url", v); err != nil {
					return err
				}
			}
			if v := c.String("listen-addr"); v != "" {
				if err := setField(s, "webhook.listen_addr", v); err != nil {
					return err
				}
			}
			if c.IsSet("listen-port") {
				if err := setField(s, "webhook.listen_port", strconv.Itoa(c.Int("listen-port"))); err != nil {
					return err
				}
			}

			if c.Bool("rotate-secret") {
				secret, err := settings.GenerateSecret()
				if err != nil {
					return err
				}
				s.Webhook.Secret = secret
				// The old secret still verifies deliveries at every repository
				// GitHub already has it configured on, until each one is
				// re-registered with the new value below.
				fmt.Fprintln(os.Stderr,
					"A new secret was minted. Run `agent-utils project register-webhook` "+
						"again for every repository so GitHub is given the new value.")
			}

			if enable {
				// Rejecting this before Save is what keeps a half-configured file
				// from ever reaching disk: an enabled daemon with nowhere to
				// deliver to is not really enabled.
				if strings.TrimSpace(s.Webhook.URL) == "" {
					return errors.New(
						"webhook.url is not set; pass --url or run " +
							"`agent-utils config set webhook.url <url>` first")
				}
				if s.Webhook.Secret == "" {
					secret, err := settings.GenerateSecret()
					if err != nil {
						return err
					}
					s.Webhook.Secret = secret
				}
				s.Webhook.Enabled = true
			}
			if disable {
				s.Webhook.Enabled = false
			}

			return settings.Save(s)
		},
	}
}
