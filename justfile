compose := "docker compose -f deploy/compose.yaml"
test_db := "postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable"
cell_dbs := "cell-1=postgres://postgres:postgres@pg-1:5432/cell?sslmode=disable,cell-2=postgres://postgres:postgres@pg-2:5432/cell?sslmode=disable,cell-3=postgres://postgres:postgres@pg-3:5432/cell?sslmode=disable,cell-4=postgres://postgres:postgres@pg-4:5432/cell?sslmode=disable"

default:
    @just --list

# Start a throwaway Postgres for the test suite.
test-db:
    docker start cellular-testpg 2>/dev/null || docker run -d --name cellular-testpg -e POSTGRES_PASSWORD=postgres -p 55432:5432 mirror.gcr.io/library/postgres:17-alpine -c max_connections=500

# Run every test, including the ones that need Postgres.
test: test-db
    TEST_DATABASE_URL='{{ test_db }}' go test -race -count=1 ./...

# Same pinned version CI uses, built with the local Go toolchain.
lint:
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2 run

# Start four cells, the router and the replicator.
up:
    {{ compose }} up -d --build --wait

down:
    {{ compose }} down -v

# Load the system and kill a cell mid-run. target: cell-2 (process) or pg-2 (its database).
chaos target="cell-2" rps="1000" duration="60s":
    {{ compose }} --profile chaos run --build --rm chaos \
        -router=http://router:8080 -rps={{ rps }} -duration={{ duration }} -victim=cell-2 \
        "-kill=docker kill cellular-auth-{{ target }}-1" "-revive=docker start cellular-auth-{{ target }}-1" \
        "-dbs={{ cell_dbs }}"
