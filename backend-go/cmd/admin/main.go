// Command admin is the operator tool (A1). It replaces the public Java
// inventory endpoints and the start-up side effects with explicit, audited
// commands. It never runs automatically.
//
//	admin check-db                            read-only readiness report (tables, migration, catalogs)
//	admin inspect-schema [-show-routine]      read-only catalog inspection (plan 8.1)
//	admin migrate -confirm                    apply additive migrations
//	admin seed-catalogs -confirm              Java seeds into empty tables only
//	admin list-players [-online]              inventory without credentials (F01/F03)
//	admin purge-receipts -confirm             delete expired idempotency receipts
//
// Uses DATABASE_DIRECT_URL when set (session features), else DATABASE_URL.
// With APP_ENV=production every mutating command also needs -allow-production.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/config"
	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: admin <check-db|inspect-schema|migrate|seed-catalogs|list-players|purge-receipts> [flags]")
	}
	if _, err := config.LoadDotEnv(".env"); err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	confirm := fs.Bool("confirm", false, "required for commands that modify data")
	allowProd := fs.Bool("allow-production", false, "required with APP_ENV=production for mutating commands")
	online := fs.Bool("online", false, "list-players: only players with a valid lease")
	showRoutine := fs.Bool("show-routine", false, "inspect-schema: print the reset_game_data body")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	mutating := cmd == "migrate" || cmd == "seed-catalogs" || cmd == "purge-receipts"
	if mutating {
		if !*confirm {
			return fmt.Errorf("%s modifies the database: re-run with -confirm after reviewing the target", cmd)
		}
		if os.Getenv("APP_ENV") == "production" && !*allowProd {
			return errors.New("APP_ENV=production: add -allow-production (and make sure a verified backup exists)")
		}
	}
	dsn := os.Getenv("DATABASE_DIRECT_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return errors.New("DATABASE_DIRECT_URL or DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, store.PoolConfig{URL: dsn, MaxConns: 2, ConnectTimeout: 10 * time.Second})
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "check-db":
		return checkDB(ctx, st, dsn)
	case "inspect-schema":
		return st.InspectSchema(ctx, os.Stdout, *showRoutine)
	case "migrate":
		applied, err := st.Migrate(ctx, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
		fmt.Printf("applied %d migration(s)\n", len(applied))
		return err
	case "seed-catalogs":
		res, err := st.SeedCatalogs(ctx)
		fmt.Printf("waiting_messages inserted: %d, impostor_word inserted: %d (non-empty tables are left untouched)\n",
			res.WaitingInserted, res.WordsInserted)
		return err
	case "list-players":
		ps, err := st.ListPlayers(ctx, *online, time.Now())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tROOM\tREADY\tONLINE\tCREATED")
		for _, p := range ps {
			room, created := "-", "-"
			if p.RoomCode != nil {
				room = *p.RoomCode
			}
			if p.CreatedAt != nil {
				created = p.CreatedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%t\t%t\t%s\n", p.ID, p.Username, room, p.Ready, p.Online, created)
		}
		return tw.Flush()
	case "purge-receipts":
		n, err := st.PurgeExpiredReceipts(ctx, time.Now(), 500)
		fmt.Printf("purged %d expired receipt(s)\n", n)
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// checkDB prints a read-only readiness report. Only the host is shown, never
// the user or password of the DSN.
func checkDB(ctx context.Context, st *store.Store, dsn string) error {
	host := "?"
	if u, err := url.Parse(dsn); err == nil {
		host = u.Hostname()
	}
	fmt.Printf("host: %s\n", host)
	r, err := st.Check(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo consultar la base de datos (revisa DATABASE_URL): %w", err)
	}
	fmt.Printf("postgres: %s\n", r.ServerVersion)
	ok := true
	if len(r.MissingTables) > 0 {
		ok = false
		fmt.Printf("FALTAN tablas de Spring Boot: %v\n", r.MissingTables)
		fmt.Println("  -> en una BD vacía de desarrollo: psql -d <bd> -f internal/testdb/baseline_hibernate_inferred.sql")
	}
	for _, t := range []string{"users", "game_session", "question", "yo_nunca_nunca", "cultura_pendeja",
		"quien_es_mas_probable", "preguntas_incomodas", "impostor_word", "waiting_messages"} {
		if n, found := r.Counts[t]; found {
			fmt.Printf("  %-22s %d filas\n", t, n)
		}
	}
	if r.MigrationApplied {
		fmt.Println("migración 0001_go_runtime: aplicada")
	} else {
		ok = false
		fmt.Println("migración 0001_go_runtime: PENDIENTE -> go run ./cmd/admin migrate -confirm")
	}
	if r.Counts["impostor_word"] == 0 || r.Counts["waiting_messages"] == 0 {
		fmt.Println("semillas: faltan palabras/mensajes -> go run ./cmd/admin seed-catalogs -confirm")
	}
	for _, t := range []string{"yo_nunca_nunca", "cultura_pendeja", "quien_es_mas_probable", "preguntas_incomodas"} {
		if n, found := r.Counts[t]; found && n == 0 {
			fmt.Printf("aviso: %s está vacía; ese juego responderá CATALOG_EMPTY\n", t)
		}
	}
	if ok {
		fmt.Println("RESULTADO: lista para el backend Go")
		return nil
	}
	return errors.New("la base de datos aún no está lista (ver arriba)")
}
