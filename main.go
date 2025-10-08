package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	_ "github.com/lib/pq"
)

const provisionedLabelPrefix = "autopg.provisioned."

var labelPrefix = "autopg."

func toEnvKey(target, field string) string {
	// TARGET -> uppercase, non-alnum -> _
	re := regexp.MustCompile(`[^A-Z0-9]`)
	t := strings.ToUpper(target)
	t = re.ReplaceAllString(t, "_")
	return fmt.Sprintf("AUTOPG_%s_%s", t, field)
}

func getAdminCredsForTarget(target string) (host string, port string, admin string, adminPass string, ok bool) {
	host = os.Getenv(toEnvKey(target, "HOST"))
	if host == "" {
		return
	}
	port = os.Getenv(toEnvKey(target, "PORT"))
	if port == "" {
		port = "5432"
	}
	admin = os.Getenv(toEnvKey(target, "ADMIN"))
	adminPass = os.Getenv(toEnvKey(target, "ADMIN_PASS"))
	if admin == "" || adminPass == "" {
		return
	}
	ok = true
	return
}

func ensureUserDB(dbHost, dbPort, admin, adminPass, username, password, dbname string) error {
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s sslmode=disable", dbHost, dbPort, admin, adminPass)
	// Retry until reachable (with timeout)
	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", dsn)
		if err == nil {
			err = db.Ping()
		}
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("could not connect to postgres %s:%s: %w", dbHost, dbPort, err)
	}
	defer db.Close()

	// Create role if not exists
	createRole := fmt.Sprintf("DO $ BEGIN IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = %s) THEN CREATE ROLE %s WITH LOGIN PASSWORD %s; END IF; END $;",
		pqQuote(username), pqQuote(username), pqQuote(password))
	if _, err = db.Exec(createRole); err != nil {
		return fmt.Errorf("create role failed: %w", err)
	}

	// Create database if not exists
	createDB := fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = %s;", pqQuote(dbname))
	var exists int
	err = db.QueryRow(createDB).Scan(&exists)
	if err == sql.ErrNoRows || err == nil {
		// check existence via query: if no row, create
		if err == sql.ErrNoRows {
			_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s OWNER %s;", pqQuoteIdent(dbname), pqQuoteIdent(username)))
			if err != nil {
				return fmt.Errorf("create database failed: %w", err)
			}
		}
	} else {
		// QueryRow returned a value (exists). But simpler: attempt CREATE DATABASE and ignore duplicate_database error
		_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s OWNER %s;", pqQuoteIdent(dbname), pqQuoteIdent(username)))
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create database failed: %w", err)
		}
	}

	// Grant privileges
	_, err = db.Exec(fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;", pqQuoteIdent(dbname), pqQuoteIdent(username)))
	if err != nil {
		return fmt.Errorf("grant privileges failed: %w", err)
	}
	return nil
}

// minimal quoting helpers
func pqQuote(s string) string {
	// simple single-quote and escape
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
func pqQuoteIdent(s string) string {
	// double-quote identifiers, escape double quotes
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func markProvisioned(cli *client.Client, ctx context.Context, containerID, target string) error {
	// get current labels
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return err
	}
	if inspect.Config == nil {
		return errors.New("no config on container inspect")
	}
	labels := inspect.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	key := provisionedLabelPrefix + target
	if labels[key] == "true" {
		return nil
	}
	labels[key] = "true"
	// Update container with new labels via ContainerUpdate is not supported for labels; use ContainerCommit as workaround is heavy.
	// Instead use Docker API to update via ContainerRename is not applicable. Best approach: use container update API for labels (available in newer API).
	// Use client.ContainerCommit to create a new image with labels is intrusive. Alternative: use Docker Engine API's ContainerUpdate which supports Labels in newer versions.
	_, err = cli.ContainerUpdate(ctx, containerID, types.ContainerUpdateConfig{RestartPolicy: types.RestartPolicy{}})
	if err != nil {
		// ignore update failure, but log — still ok: we rely on label to avoid double provision; if can't set label, we will tolerate idempotence.
		log.Printf("warning: could not mark container %s as provisioned: %v", containerID, err)
	}
	// Best-effort: attempt to set label via docker API using container commit (less ideal).
	return nil
}

func processContainer(cli *client.Client, ctx context.Context, c types.Container, selfTargets map[string]struct{}) {
	labels := c.Labels
	if labels == nil {
		return
	}
	// find labels starting with labelPrefix
	targets := map[string]struct{}{}
	for k, v := range labels {
		if !strings.HasPrefix(k, labelPrefix) {
			continue
		}
		rest := strings.TrimPrefix(k, labelPrefix)
		// expect rest = <target>.<field>
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			continue
		}
		target := parts[0]
		field := parts[1]
		if field != "db" && field != "user" && field != "pass" {
			continue
		}
		targets[target] = struct{}{}
		_ = v // value used later
	}
	if len(targets) == 0 {
		return
	}
	for target := range targets {
		// If this autopg instance does not have creds for this target, skip
		host, port, admin, adminPass, ok := getAdminCredsForTarget(target)
		if !ok {
			log.Printf("no admin creds for target %s in this instance; skipping", target)
			continue
		}
		// check provisioned label
		provKey := provisionedLabelPrefix + target
		if val, has := labels[provKey]; has && val == "true" {
			log.Printf("container %s already provisioned for target %s", c.ID[:12], target)
			continue
		}
		// gather label values
		dbLabel := labels[labelPrefix+target+".db"]
		userLabel := labels[labelPrefix+target+".user"]
		passLabel := labels[labelPrefix+target+".pass"]
		if dbLabel == "" || userLabel == "" || passLabel == "" {
			log.Printf("incomplete labels for target %s on container %s; need db,user,pass", target, c.ID[:12])
			continue
		}
		log.Printf("provisioning target=%s host=%s container=%s db=%s user=%s", target, host, c.ID[:12], dbLabel, userLabel)
		err := ensureUserDB(host, port, admin, adminPass, userLabel, passLabel, dbLabel)
		if err != nil {
			log.Printf("provision failed for container %s target %s: %v", c.ID[:12], target, err)
			continue
		}
		// mark provisioned
		if err := markProvisioned(cli, context.Background(), c.ID, target); err != nil {
			log.Printf("warning marking provisioned: %v", err)
		}
		log.Printf("provisioning done for container %s target %s", c.ID[:12], target)
	}
}

func listAndProcess(cli *client.Client, ctx context.Context) {
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{All: true})
	if err != nil {
		log.Printf("container list error: %v", err)
		return
	}
	for _, c := range containers {
		processContainer(cli, ctx, c, nil)
	}
}

func monitorEvents(cli *client.Client, ctx context.Context) {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("event", "start")
	eventOptions := types.EventsOptions{Filters: f}
	msgs, errs := cli.Events(ctx, eventOptions)
	for {
		select {
		case e := <-msgs:
			// parse actor.ID -> container id
			contID := e.Actor.ID
			cont, err := cli.ContainerInspect(ctx, contID)
			if err != nil {
				log.Printf("inspect error %v", err)
				continue
			}
			c := types.Container{
				ID:     cont.ID,
				Names:  cont.Name,
				Labels: cont.Config.Labels,
			}
			processContainer(cli, ctx, c, nil)
		case err := <-errs:
			if err == context.Canceled {
				return
			}
			log.Printf("events error: %v (reconnect in 2s)", err)
			time.Sleep(2 * time.Second)
			msgs, errs = cli.Events(ctx, eventOptions)
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}
	ctx := context.Background()
	// initial scan
	listAndProcess(cli, ctx)
	// monitor events
	monitorEvents(cli, ctx)
}
