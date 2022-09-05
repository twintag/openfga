package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	_ "github.com/go-sql-driver/mysql"

	"github.com/openfga/openfga/assets"
	"github.com/openfga/openfga/pkg/id"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

const (
	mySQLImage = "mysql:latest"
)

type mySQLTestContainer struct {
	conn  *sql.DB
	addr  string
	creds string
}

// NewMySQLTestContainer returns an implementation of the DatastoreTestContainer interface
// for MySQL.
func NewMySQLTestContainer() *mySQLTestContainer {
	return &mySQLTestContainer{}
}

// RunMySQLTestContainer runs a MySQL container, connects to it, and returns a
// bootstrapped implementation of the DatastoreTestContainer interface wired up for the
// MySQL datastore engine.
func (m *mySQLTestContainer) RunMySQLTestContainer(t testing.TB) DatastoreTestContainer {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	require.NoError(t, err)

	reader, err := dockerClient.ImagePull(context.Background(), mySQLImage, types.ImagePullOptions{})
	require.NoError(t, err)

	_, err = io.Copy(io.Discard, reader) // consume the image pull output to make sure it's done
	require.NoError(t, err)

	containerCfg := container.Config{
		Env: []string{
			"MYSQL_DATABASE=defaultdb",
			"MYSQL_ROOT_PASSWORD=secret",
		},
		ExposedPorts: nat.PortSet{
			nat.Port("3306/tcp"): {},
		},
		Image: mySQLImage,
	}

	hostCfg := container.HostConfig{
		AutoRemove:      true,
		PublishAllPorts: false,
		PortBindings: nat.PortMap{
			"3306/tcp": []nat.PortBinding{
				{HostPort: "3306"},
			},
		},
	}

	ulid, err := id.NewString()
	require.NoError(t, err)

	name := fmt.Sprintf("mysql-%s", ulid)

	cont, err := dockerClient.ContainerCreate(context.Background(), &containerCfg, &hostCfg, nil, nil, name)
	require.NoError(t, err, "failed to create mysql docker container")

	stopContainer := func() {

		timeout := 5 * time.Second

		err := dockerClient.ContainerStop(context.Background(), cont.ID, &timeout)
		if err != nil && !client.IsErrNotFound(err) {
			t.Fatalf("failed to stop mysql container: %v", err)
		}
	}

	err = dockerClient.ContainerStart(context.Background(), cont.ID, types.ContainerStartOptions{})
	if err != nil {
		stopContainer()
		t.Fatalf("failed to start mysql container: %v", err)
	}

	containerJSON, err := dockerClient.ContainerInspect(context.Background(), cont.ID)
	require.NoError(t, err)

	p, ok := containerJSON.NetworkSettings.Ports["3306/tcp"]
	if !ok || len(p) == 0 {
		t.Fatalf("failed to get host port mapping from mysql container")
	}

	// spin up a goroutine to survive any test panics to expire/stop the running container
	go func() {
		time.Sleep(expireTimeout)

		err := dockerClient.ContainerStop(context.Background(), cont.ID, nil)
		if err != nil && !client.IsErrNotFound(err) {
			t.Fatalf("failed to expire mysql container: %v", err)
		}
	}()

	t.Cleanup(func() {
		stopContainer()
	})

	mySQLTestContainer := &mySQLTestContainer{
		addr:  fmt.Sprintf("localhost:%s", p[0].HostPort),
		creds: "root:secret",
	}

	uri := fmt.Sprintf("%s@tcp(%s)/defaultdb?parseTime=true", mySQLTestContainer.creds, mySQLTestContainer.addr)

	backoffPolicy := backoff.NewExponentialBackOff()
	backoffPolicy.MaxElapsedTime = 60 * time.Second

	err = backoff.Retry(
		func() error {
			var err error

			mySQLTestContainer.conn, err = sql.Open("mysql", uri)
			if err != nil {
				return err
			}
			err = mySQLTestContainer.conn.Ping()
			if err != nil {
				return err
			}

			return nil
		},
		backoffPolicy,
	)
	if err != nil {
		stopContainer()
		t.Fatalf("failed to connect to mysql container: %v", err)
	}

	db, err := sql.Open("mysql", uri)
	require.NoError(t, err)

	goose.SetLogger(goose.NopLogger())

	err = goose.SetDialect("mysql")
	require.NoError(t, err)

	goose.SetBaseFS(assets.EmbedMigrations)

	err = goose.Up(db, assets.MySQLMigrationDir)
	require.NoError(t, err)

	return mySQLTestContainer
}

// GetConnectionURI returns the mysql connection uri for the running mysql test container.
func (m *mySQLTestContainer) GetConnectionURI() string {
	return fmt.Sprintf(
		"%s@tcp(%s)/%s?parseTime=true",
		m.creds,
		m.addr,
		"defaultdb",
	)
}
