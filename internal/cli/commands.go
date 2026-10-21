// Package cli implements the moose command-line client over the REST API.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/change-fabric/moose/internal/api"
	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/core"
)

// Env is the process context the commands run in.
type Env struct {
	Getenv  func(string) string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	HTTP    *http.Client
	Version string
}

type app struct {
	env      Env
	credPath string
}

// NewRoot builds the moose command tree.
func NewRoot(env Env) *cobra.Command {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	a := &app{env: env}
	root := &cobra.Command{
		Use:           "moose",
		Short:         "moose CLI: search and read data shared with your moose hub",
		Long:          "moose calls the moose REST API. Credentials live in ~/.config/moose/credentials.json;\nMOOSE_URL and MOOSE_PAT override them.\n\nExit codes: 0 ok, 1 usage, 2 auth (401/403), 3 not found, 4 upstream/server.",
		Version:       env.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			p, err := CredentialsPath(a.env.Getenv)
			a.credPath = p
			return err
		},
	}
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &ExitError{Code: ExitUsage, Err: err} })
	root.AddCommand(a.loginCmd(), a.whoamiCmd(), a.sourcesCmd(), a.searchCmd(), a.getCmd(), a.tokenCmd(), a.userCmd(), a.auditCmd())
	return root
}

// client resolves the URL and token: env overrides the credential file.
func (a *app) client() (*Client, error) {
	creds, err := loadCredentials(a.credPath)
	if err != nil {
		return nil, usageErr("read %s: %v", a.credPath, err)
	}
	if v := a.env.Getenv("MOOSE_URL"); v != "" {
		creds.URL = v
	}
	if v := a.env.Getenv("MOOSE_PAT"); v != "" {
		creds.Token = v
	}
	return &Client{BaseURL: creds.URL, Token: creds.Token, HTTP: a.env.HTTP, UserAgent: "moose/" + a.env.Version}, nil
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) table(header string, rows func(w io.Writer)) {
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	rows(tw)
	_ = tw.Flush()
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErr("%s takes no arguments", cmd.CommandPath())
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErr("%s needs %d argument(s); see --help", cmd.CommandPath(), n)
		}
		return nil
	}
}

func (a *app) loginCmd() *cobra.Command {
	var hub, token string
	cmd := &cobra.Command{
		Use:   "login --url URL [--token T]",
		Short: "Save a hub URL and token after checking them with /v1/whoami",
		Long:  "Save a hub URL and token. Without --token, the token is read from stdin.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if hub == "" {
				return usageErr("--url is required")
			}
			if token == "" {
				line, err := bufio.NewReader(a.env.Stdin).ReadString('\n')
				if err != nil && err != io.EOF {
					return usageErr("read token from stdin: %v", err)
				}
				token = strings.TrimSpace(line)
			}
			if token == "" {
				return usageErr("no token given (use --token or stdin)")
			}
			c := &Client{BaseURL: hub, Token: token, HTTP: a.env.HTTP, UserAgent: "moose/" + a.env.Version}
			var who api.WhoAmI
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/whoami", nil, nil, &who); err != nil {
				return err
			}
			if err := saveCredentials(a.credPath, Credentials{URL: hub, Token: token}); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stdout, "logged in to %s as %s (%s)\n", hub, who.Login, who.Role)
			return nil
		},
	}
	cmd.Flags().StringVar(&hub, "url", "", "hub URL, e.g. https://moose.<tailnet>.ts.net")
	cmd.Flags().StringVar(&token, "token", "", "personal access token (default: read stdin)")
	return cmd
}

func (a *app) whoamiCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Print the authenticated principal",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var who api.WhoAmI
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/whoami", nil, nil, &who); err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(who)
			}
			fmt.Fprintf(a.env.Stdout, "login:    %s\nrole:     %s\ntoken:    %s\n", who.Login, who.Role, who.TokenID)
			if who.Identity.Login != "" {
				fmt.Fprintf(a.env.Stdout, "tailnet:  %s (%s)\n", who.Identity.Login, who.Identity.Node)
			} else if len(who.Identity.Tags) > 0 {
				fmt.Fprintf(a.env.Stdout, "tailnet:  %s (%s)\n", strings.Join(who.Identity.Tags, ","), who.Identity.Node)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) sourcesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List connected data sources",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Sources []api.SourceInfo `json:"sources"`
			}
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/sources", nil, nil, &out); err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(out)
			}
			a.table("ID\tNAME\tHEALTHY\tDETAIL", func(w io.Writer) {
				for _, s := range out.Sources {
					fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", s.ID, s.Name, s.Healthy, s.Detail)
				}
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) searchCmd() *cobra.Command {
	var limit int
	var source, pageToken string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Search shared documents",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			q := url.Values{"q": {strings.Join(args, " ")}}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if source != "" {
				q.Set("source", source)
			}
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			var res api.ResultPage
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/search", q, nil, &res); err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(res)
			}
			a.printRefs(res)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max results (1-100, default 20)")
	cmd.Flags().StringVar(&source, "source", "", "source id (default: all)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "page token from a previous search")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) printRefs(res api.ResultPage) {
	a.table("TITLE\tID\tMODIFIED\tURL", func(w io.Writer) {
		for _, r := range res.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Title, r.ID, r.Modified.Format(time.DateOnly), r.URL)
		}
	})
	if res.NextPageToken != "" {
		fmt.Fprintf(a.env.Stderr, "next page: --page-token %s\n", res.NextPageToken)
	}
}

func (a *app) getCmd() *cobra.Command {
	var source string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get ID",
		Short: "Print a document's content to stdout",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var doc core.Document
			path := "/v1/documents/" + url.PathEscape(source) + "/" + url.PathEscape(args[0])
			if err := c.do(cmd.Context(), http.MethodGet, path, nil, nil, &doc); err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(doc)
			}
			if doc.ContentType == "metadata-only" {
				fmt.Fprintf(a.env.Stderr, "%s (%s) has no text content; open %s\n", doc.Title, doc.MimeType, doc.URL)
				return nil
			}
			fmt.Fprint(a.env.Stdout, doc.Content)
			if doc.Content != "" && !strings.HasSuffix(doc.Content, "\n") {
				fmt.Fprintln(a.env.Stdout)
			}
			if doc.Truncated {
				fmt.Fprintln(a.env.Stderr, "note: content truncated")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "gdrive", "source id")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage personal access tokens", Args: noArgs}

	var name, owner string
	var days int
	create := &cobra.Command{
		Use:   "create --name N [--owner O] [--expires-in-days D]",
		Short: "Create a token and print it once",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return usageErr("--name is required")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			body := map[string]any{"name": name}
			if owner != "" {
				body["owner"] = owner
			}
			if cmd.Flags().Changed("expires-in-days") {
				body["expires_in_days"] = days
			}
			var out api.CreatedToken
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/tokens", nil, body, &out); err != nil {
				return err
			}
			fmt.Fprintln(a.env.Stdout, out.Token)
			fmt.Fprintf(a.env.Stderr, "token %s for %s%s; it is shown only once\n", out.ID, out.Owner, expiry(out.Expires))
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "token name")
	create.Flags().StringVar(&owner, "owner", "", "owner login or tag:x (admin only when not yourself)")
	create.Flags().IntVar(&days, "expires-in-days", 90, "days until expiry; 0 = never (admin only)")

	var bURL, bName string
	bootstrap := &cobra.Command{
		Use:   "bootstrap --url URL --name N",
		Short: "Get your first token from a hub and save credentials",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if bURL == "" || bName == "" {
				return usageErr("--url and --name are required")
			}
			c := &Client{BaseURL: bURL, HTTP: a.env.HTTP, UserAgent: "moose/" + a.env.Version}
			var out auth.BootstrapResponse
			if err := c.do(cmd.Context(), http.MethodPost, auth.BootstrapPath, nil, map[string]string{"name": bName}, &out); err != nil {
				return err
			}
			if err := saveCredentials(a.credPath, Credentials{URL: bURL, Token: out.Token}); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stdout, "token %s issued to %s%s; saved to %s\n", out.ID, out.Owner, expiry(out.Expires), a.credPath)
			return nil
		},
	}
	bootstrap.Flags().StringVar(&bURL, "url", "", "hub URL")
	bootstrap.Flags().StringVar(&bName, "name", "", "token name")

	var all bool
	var listOwner string
	list := &cobra.Command{
		Use:   "list [--all]",
		Short: "List tokens (yours; --all or --owner for admins)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if all {
				q.Set("all", "1")
			} else if listOwner != "" {
				q.Set("owner", listOwner)
			}
			var out struct {
				Tokens []api.TokenInfo `json:"tokens"`
			}
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/tokens", q, nil, &out); err != nil {
				return err
			}
			a.table("ID\tOWNER\tNAME\tCREATED\tLAST USED\tEXPIRES\tREVOKED", func(w io.Writer) {
				for _, t := range out.Tokens {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n", t.ID, t.Owner, t.Name, t.Created.Format(time.DateOnly), when(t.LastUsed), when(t.Expires), t.Revoked)
				}
			})
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "all owners (admin)")
	list.Flags().StringVar(&listOwner, "owner", "", "one owner (admin)")

	revoke := &cobra.Command{
		Use:   "revoke ID",
		Short: "Revoke a token",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if err := c.do(cmd.Context(), http.MethodDelete, "/v1/tokens/"+url.PathEscape(args[0]), nil, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stdout, "revoked %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(create, bootstrap, list, revoke)
	return cmd
}

func (a *app) userCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Manage users (admin)", Args: noArgs}
	var role string
	add := &cobra.Command{
		Use:   "add LOGIN [--role member|admin]",
		Short: "Add or update a user",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role != auth.RoleMember && role != auth.RoleAdmin {
				return usageErr("--role must be member or admin")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var u api.UserInfo
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/users", nil, map[string]string{"login": args[0], "role": role}, &u); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stdout, "user %s (%s)\n", u.Login, u.Role)
			return nil
		},
	}
	add.Flags().StringVar(&role, "role", auth.RoleMember, "member or admin")
	list := &cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var out struct {
				Users []api.UserInfo `json:"users"`
			}
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/users", nil, nil, &out); err != nil {
				return err
			}
			a.table("LOGIN\tROLE\tCREATED\tDISABLED", func(w io.Writer) {
				for _, u := range out.Users {
					fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", u.Login, u.Role, u.Created.Format(time.DateOnly), u.Disabled)
				}
			})
			return nil
		},
	}
	cmd.AddCommand(add, list)
	return cmd
}

func (a *app) auditCmd() *cobra.Command {
	var since string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "audit [--since 24h]",
		Short: "Show recent audit events (admin)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			q := url.Values{"since": {since}}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var out struct {
				Events []api.AuditInfo `json:"events"`
			}
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/audit", q, nil, &out); err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(out)
			}
			a.table("AT\tACTOR\tSURFACE\tACTION\tTARGET\tSTATUS\tMS\tDETAIL", func(w io.Writer) {
				for _, e := range out.Events {
					target := strings.Trim(e.Source+"/"+e.DocID, "/")
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", e.At.Local().Format(time.DateTime), e.Actor, e.Surface, e.Action, target, e.Status, e.LatencyMS, e.Detail)
				}
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "24h", "duration (24h) or RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", 0, "max events (default 100)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format(time.DateOnly)
}

func expiry(t *time.Time) string {
	if t == nil {
		return ", no expiry"
	}
	return ", expires " + t.Local().Format(time.DateOnly)
}
