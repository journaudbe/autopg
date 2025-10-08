# autopg

autopg — provision PostgreSQL users & databases from Docker container labels.

autopg watches the Docker API (/var/run/docker.sock), reads app container labels in the form
autopg.<target>.db, autopg.<target>.user, autopg.<target>.pass and, for targets for which it has admin
credentials via environment variables, creates the role and database on the target PostgreSQL server.
Designed in Go, idempotent and easy to run as a Docker service.

## How it works
- App containers include labels: `autopg.<target>.db`, `autopg.<target>.user`, `autopg.<target>.pass`.
- autopg instance has admin credentials per target via environment variables:
  - `AUTOPG_<TARGET>_HOST`
  - `AUTOPG_<TARGET>_PORT` (optional, default 5432)
  - `AUTOPG_<TARGET>_ADMIN`
  - `AUTOPG_<TARGET>_ADMIN_PASS`
  - `<TARGET>` is uppercased and non-alphanumeric characters are replaced with `_`.
  - Example: target `monserverpostgre` → `AUTOPG_MONSERVERPOSTGRE_HOST`, etc.
- autopg scans existing containers at startup and listens to container.start events.
- For each target found on a container, if the autopg instance has admin credentials for that target,
  autopg will:
  - create role (user) if not exists,
  - create database if not exists and set owner,
  - grant privileges on database to the user.
- autopg attempts a best-effort marking of the container with label `autopg.provisioned.<target>=true`
  to avoid re-provisioning; operations are idempotent so lack of marking is safe.

## Repository contents
- main.go — Go implementation
- Dockerfile — multi-stage build producing a small runtime image
- docker-compose.yml — example with multiple PostgreSQL servers and an app container using labels
- README.md — this file

## Quick start (example docker-compose)
Example excerpt:
```yaml
version: "3.8"
services:
  postgres_a:
    image: postgres:15
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: secretA
    volumes:
      - pgdata_a:/var/lib/postgresql/data

  postgres_b:
    image: postgres:15
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: secretB
    volumes:
      - pgdata_b:/var/lib/postgresql/data

  autopg:
    build: .
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      AUTOPG_MYSERVERPG_HOST: "postgres_a"
      AUTOPG_MYSERVERPG_PORT: "5432"
      AUTOPG_MYSERVERPG_ADMIN: "postgres"
      AUTOPG_MYSERVERPG_ADMIN_PASS: "secretA"

      AUTOPG_OTHERPG_HOST: "postgres_b"
      AUTOPG_OTHERPG_PORT: "5432"
      AUTOPG_OTHERPG_ADMIN: "postgres"
      AUTOPG_OTHERPG_ADMIN_PASS: "secretB"

  app:
    image: alpine:3.18
    command: ["sleep","3600"]
    labels:
      autopg.myserverpg.db: "appdb"
      autopg.myserverpg.user: "appuser"
      autopg.myserverpg.pass: "apppass"
      autopg.otherpg.db: "otherdb"
      autopg.otherpg.user: "otheruser"
      autopg.otherpg.pass: "otherpass"
    depends_on:
      - postgres_a
      - postgres_b

volumes:
  pgdata_a:
  pgdata_b:
```

Build and run:
- docker-compose build
- docker-compose up -d

Behavior:
- autopg scans existing containers on start, then listens for new containers.
- For each container with matching labels and available admin creds, it provisions the user+db.

## Environment variables per target
- Host: `AUTOPG_<TARGET>_HOST`
- Port (optional): `AUTOPG_<TARGET>_PORT` (default 5432)
- Admin user: `AUTOPG_<TARGET>_ADMIN`
- Admin password: `AUTOPG_<TARGET>_ADMIN_PASS`

## Notes and recommendations
- Admin credentials must be provided only to autopg (not in labels). Use Docker secrets if available.
- The code uses `sslmode=disable` by default; adapt the connection string to enable TLS as needed.
- autopg requires access to the Docker socket. To reduce risk, mount the socket read-only where possible and run autopg in a restricted environment.
- Provisioning is idempotent: repeated runs are safe.
- Marking containers as provisioned is best-effort; if your Docker daemon/version doesn't allow label updates, operations will still be safe but may re-run.

## Limitations
- Requires Docker socket access.
- Default DB connection does not enforce TLS.
- Marking labels on containers is not guaranteed on all daemon versions; state can be adapted to use a local sqlite file or external store if preferred.

## Contributing
- Issues and PRs welcome.
- Suggested improvements: TLS configuration, Docker socket read-only handling, optional state backend (sqlite), tests.