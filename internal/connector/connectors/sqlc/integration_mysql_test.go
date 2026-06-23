//go:build integration

package sqlc

import (
	"testing"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	"github.com/dolthub/go-mysql-server/server"
	gsql "github.com/dolthub/go-mysql-server/sql"
)

// TestMySQLIntegration runs an in-process MySQL-compatible server
// (go-mysql-server, pure Go, no external binary) and exercises the connector
// through the standard MySQL wire driver.
func TestMySQLIntegration(t *testing.T) {
	const (
		dbName = "turntable"
		addr   = "localhost:33306"
	)

	db := memory.NewDatabase(dbName)
	db.BaseDatabase.EnablePrimaryKeyIndexes()
	pro := memory.NewDBProvider(db)
	engine := sqle.NewDefault(pro)

	cfg := server.Config{Protocol: "tcp", Address: addr}
	s, err := server.NewServer(cfg, engine, gsql.NewContext, memory.NewSessionBuilder(pro), nil)
	if err != nil {
		t.Fatalf("new mysql server: %v", err)
	}
	go func() { _ = s.Start() }()
	defer func() { _ = s.Close() }()

	dsn := "root:@tcp(" + addr + ")/" + dbName
	seedInventory(t, "mysql", dsn, "DOUBLE")
	checkConnector(t, "mysql", dsn)
}
