package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/chatui"
)

func runKeys(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: gateway keys <create|list|show|use|revoke> [flags]")
	}
	action := args[0]
	switch action {
	case "create", "list", "show", "use", "revoke":
	default:
		return errors.New("unknown key command; use create, list, show, use, or revoke")
	}
	defaultDir, err := auth.DefaultDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("keys "+action, flag.ContinueOnError)
	flags.SetOutput(errOut)
	dir := flags.String("config-dir", defaultDir, "Directory containing server hashes and client credentials")
	var user, baseURL, id, chatURL string
	if action == "create" {
		flags.StringVar(&user, "user", "", "User ID (required)")
		flags.StringVar(&baseURL, "base-url", "http://127.0.0.1:8000", "Gateway origin for automatic client key reuse")
		flags.StringVar(&chatURL, "chat-url", "", "Chat link to print; defaults to --base-url/chat/. Public links require HTTPS")
	}
	if action == "show" || action == "use" || action == "revoke" {
		flags.StringVar(&id, "id", "", "Key ID; show defaults to the selected key")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (action == "use" || action == "revoke") && id == "" {
		return errors.New("--id is required")
	}
	if action == "create" {
		chatURL, err = chatui.Link(baseURL, chatURL)
		if err != nil {
			return err
		}
	}
	store, err := auth.Open(*dir)
	if err != nil {
		return err
	}
	switch action {
	case "create":
		record, err := store.Create(user, baseURL)
		if err != nil {
			return err
		}
		token, err := store.Show(record.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Created key %s for user %s.\nSaved and selected in %s\nAPI token: %s\nCopy the full API token into Postman's Bearer Token field.\n", record.ID, record.UserID, store.CredentialsPath(), token)
		fmt.Fprintf(out, "Chat UI: %s\nOpen the chat link and paste the full API token to connect.\n", chatURL)
	case "list":
		records, err := store.List()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "KEY ID\tUSER\tSTATUS")
		for _, record := range records {
			status := "active"
			if record.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\n", record.ID, record.UserID, status)
		}
	case "show":
		token, err := store.Show(id)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, token)
	case "use":
		if err := store.Use(id); err != nil {
			return err
		}
		fmt.Fprintf(out, "Selected key %s.\n", id)
	case "revoke":
		if err := store.Revoke(id); err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked key %s; new requests using it will be rejected.\n", id)
	}
	return nil
}
