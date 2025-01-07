package main

import (
	"context"
	"fmt"
	"github.com/thanos-io/objstore/providers/filesystem"
	"log"
	"log/slog"
	"time"

	"github.com/slatedb/slatedb-go/slatedb"
)

func main() {
	bucket, err := filesystem.NewBucket("/tmp")
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, _ := slatedb.Open(ctx, "testDB", bucket)

	key := []byte("key1")
	value := []byte("value1")

	db.Put(key, value)
	fmt.Println("Put:", string(key), string(value))

	data, _ := db.Get(ctx, key)
	fmt.Println("Get:", string(key), string(data))

	db.Delete(key)
	_, err = db.Get(ctx, key)
	if err != nil && err.Error() == "key not found" {
		fmt.Println("Delete:", string(key))
	} else {
		slog.Error("Unable to delete", "error", err)
	}

	if err := db.Close(); err != nil {
		slog.Error("Error closing db", "error", err)
	}
}
