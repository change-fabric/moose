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

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/config"
)

// runKillSwitch implements `moose-email kill-switch on [reason] | off | status`.
// It reads and writes the same store the server uses, and the server checks
// the switch on every send request and again at dispatch, so flipping it takes
// effect at once without a redeploy.
func runKillSwitch(ctx context.Context, args []string, out io.Writer, loadAWS func(context.Context) (aws.Config, error)) error {
	if len(args) == 0 {
		return errors.New("usage: moose-email kill-switch on [provider=<name>] [reason] | off [provider=<name>] | status [provider=<name>]")
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
	if len(args) == 0 {
		return errors.New("usage: moose-email kill-switch on [provider=<name>] [reason] | off [provider=<name>] | status [provider=<name>]")
	}
	cmd, rest := args[0], args[1:]
	// provider=<name> scopes the switch to one provider (the send engine turns
	// the microsoft one on itself after a sent_mismatch).
	prov := ""
	var words []string
	for _, a := range rest {
		if v, ok := strings.CutPrefix(a, "provider="); ok {
			prov = strings.TrimSpace(v)
			continue
		}
		words = append(words, a)
	}
	if prov != "" && prov != store.ProviderGoogle && prov != store.ProviderMicrosoft {
		return fmt.Errorf("unknown provider %q; use %s or %s", prov, store.ProviderGoogle, store.ProviderMicrosoft)
	}
	label := "kill switch"
	set := func(on bool, reason string) error {
		if prov != "" {
			return st.SetProviderKillSwitch(ctx, prov, on, reason, now)
		}
		return st.SetKillSwitch(ctx, on, reason, now)
	}
	if prov != "" {
		label = prov + " kill switch"
	}
	switch cmd {
	case "on":
		if err := set(true, strings.TrimSpace(strings.Join(words, " "))); err != nil {
			return err
		}
		if prov != "" {
			fmt.Fprintf(out, "%s is ON: no message will be sent from %s accounts\n", label, prov)
		} else {
			fmt.Fprintln(out, "kill switch is ON: no message will be sent")
		}
	case "off":
		if err := set(false, ""); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s is off\n", label)
	case "status":
		var ks store.KillState
		var err error
		if prov != "" {
			ks, err = st.ProviderKillSwitch(ctx, prov)
		} else {
			ks, err = st.KillSwitch(ctx)
		}
		if err != nil {
			return err
		}
		if ks.On {
			fmt.Fprintf(out, "%s is ON since %s %s\n", label, ks.At.Format(time.RFC3339), ks.Reason)
		} else {
			fmt.Fprintf(out, "%s is off\n", label)
		}
	default:
		return errors.New("usage: moose-email kill-switch on [provider=<name>] [reason] | off [provider=<name>] | status [provider=<name>]")
	}
	return nil
}
