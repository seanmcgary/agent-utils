package main

import (
	"context"

	"github.com/seanmcgary/agent-utils/internal/metrics"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/urfave/cli/v3"
)

// statsdFlags are the global flags that turn on DogStatsD metrics.
//
// They are root flags, and urfave/cli v3 makes a root flag persistent, so
// they may be given before the subcommand or after it: `agent-utils
// --statsd-addr 127.0.0.1:8125 listener run` and `agent-utils listener run
// --statsd-addr 127.0.0.1:8125` both reach configureMetrics.
//
// Each has an environment source, and the environment is the form that
// matters most in practice. A cron line can export it once for every tick,
// and runner.Spawn writes it into the detached runner's environment so the
// runner -- the process that records a dispatch's outcome -- emits under the
// same configuration as the tick that started it.
func statsdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "statsd-addr",
			Usage:   "DogStatsD agent host:port to send metrics to; empty disables metrics",
			Sources: cli.EnvVars(metrics.EnvAddr),
		},
		&cli.StringFlag{
			Name:    "statsd-prefix",
			Usage:   "prefix prepended to every metric name",
			Value:   metrics.DefaultPrefix,
			Sources: cli.EnvVars(metrics.EnvPrefix),
		},
		&cli.StringFlag{
			Name:    "statsd-tags",
			Usage:   "comma-separated tags added to every metric, e.g. env:prod,host:mini",
			Sources: cli.EnvVars(metrics.EnvTags),
		},
	}
}

// configureMetrics is the root command's Before. It runs for every
// subcommand, so every process this binary starts -- a tick, the listener,
// the detached runner -- is configured by the same rule.
//
// An invalid value fails the command. A typo in an address the operator just
// typed should stop them at the terminal rather than leave them reading an
// empty dashboard; metrics.Configure explains why an address that merely does
// not resolve is a warning instead.
func configureMetrics(ctx context.Context, c *cli.Command) (context.Context, error) {
	return ctx, metrics.Configure(metricsConfig(c))
}

// projectNameByID returns the registered name of the project with this id, or
// "" when there is none.
//
// It matches the id exactly rather than going through registry.Find, which
// tries a name match first: an id is all the runner was given, and a project
// whose NAME happened to equal another's id must not win.
func projectNameByID(id string) string {
	projects, err := registry.List()
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.ID == id {
			return p.Name
		}
	}
	return ""
}

func metricsConfig(c *cli.Command) metrics.Config {
	return metrics.Config{
		Addr:   c.String("statsd-addr"),
		Prefix: c.String("statsd-prefix"),
		Tags:   metrics.ParseTags(c.String("statsd-tags")),
	}
}
