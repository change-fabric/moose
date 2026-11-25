package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/email/store"
)

// runKillSwitch implements `moose-email kill-switch on [reason] | off | status`.
// It reads and writes the same store the server uses, and the server checks
// the switch on every send request and again at dispatch, so flipping it takes
// effect at once without a redeploy.
func runKillSwitch(ctx context.Context, args []string, out io.Writer, loadAWS func(context.Context) (aws.Config, error)) error {
	if len(args) == 0 {
		return errors.New("usage: moose-email kill-switch on [reason] | off | status")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	var awsCfg *aws.Config
	st, err := openStore(ctx, cfg, func() (aws.Config, error) {
		if awsCfg == nil {
			c, err := loadAWS(ctx)
			if err != nil {
				return aws.Config{}, fmt.Errorf("aws config: %w", err)
			}
			awsCfg = &c
		}
		return *awsCfg, nil
	})
	if err != nil {
		return err
	}
	defer st.Close()
	return killSwitch(ctx, st, args, out, time.Now())
}

func killSwitch(ctx context.Context, st store.Store, args []string, out io.Writer, now time.Time) error {
	switch args[0] {
	case "on":
		reason := strings.TrimSpace(strings.Join(args[1:], " "))
		if err := st.SetKillSwitch(ctx, true, reason, now); err != nil {
			return err
		}
		fmt.Fprintln(out, "kill switch is ON: no message will be sent")
	case "off":
		if err := st.SetKillSwitch(ctx, false, "", now); err != nil {
			return err
		}
		fmt.Fprintln(out, "kill switch is off")
	case "status":
		ks, err := st.KillSwitch(ctx)
		if err != nil {
			return err
		}
		if ks.On {
			fmt.Fprintf(out, "kill switch is ON since %s %s\n", ks.At.Format(time.RFC3339), ks.Reason)
		} else {
			fmt.Fprintln(out, "kill switch is off")
		}
	default:
		return errors.New("usage: moose-email kill-switch on [reason] | off | status")
	}
	return nil
}
