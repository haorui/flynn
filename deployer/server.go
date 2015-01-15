package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/bgentry/que-go"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/jackc/pgx"
	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/deployer/strategies"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/shutdown"
)

var q *que.Client
var db *postgres.DB
var client *controller.Client

func main() {
	var err error
	client, err = controller.NewClient("", os.Getenv("CONTROLLER_AUTH_KEY"))
	if err != nil {
		log.Fatalln("Unable to create controller client:", err)
	}

	if err := discoverd.Register("flynn-deployer", ":"+os.Getenv("PORT")); err != nil {
		log.Fatal(err)
	}

	postgres.Wait("")
	db, err = postgres.Open("", "")
	if err != nil {
		log.Fatal(err)
	}

	if err := migrateDB(db.DB); err != nil {
		log.Fatal(err)
	}

	pgxcfg, err := pgx.ParseURI(fmt.Sprintf("http://%s:%s@%s/%s", os.Getenv("PGUSER"), os.Getenv("PGPASSWORD"), db.Addr(), os.Getenv("PGDATABASE")))
	if err != nil {
		log.Fatal(err)
	}

	pgxpool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig:   pgxcfg,
		AfterConnect: que.PrepareStatements,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pgxpool.Close()

	q = que.NewClient(pgxpool)
	wm := que.WorkMap{
		"Deployment": handleJob,
	}

	workers := que.NewWorkerPool(q, wm, 10)
	go workers.Start()
	shutdown.BeforeExit(func() { workers.Shutdown() })

	<-make(chan bool) // block and keep running
}

func handleJob(job *que.Job) (e error) {
	var args ct.DeployID
	if err := json.Unmarshal(job.Args, &args); err != nil {
		// TODO: log error
		return err
	}
	id := args.ID
	deployment, err := client.GetDeployment(id)
	if err != nil {
		// TODO: log error
		return err
	}
	// for recovery purposes, fetch old formation
	f, err := client.GetFormation(deployment.AppID, deployment.OldReleaseID)
	if err != nil {
		// TODO: log error
		return err
	}
	strategyFunc, err := strategy.Get(deployment.Strategy)
	if err != nil {
		// TODO: log error
		return err
	}
	events := make(chan ct.DeploymentEvent)
	defer close(events)
	go func() {
		for ev := range events {
			ev.DeploymentID = deployment.ID
			if err := sendDeploymentEvent(ev); err != nil {
				log.Print(err)
			}
		}
	}()
	defer func() {
		// rollback failed deploy
		if e != nil {
			events <- ct.DeploymentEvent{
				ReleaseID: deployment.NewReleaseID,
				Status:    "failed",
			}
			if e = client.PutFormation(f); e != nil {
				return
			}
			if e = client.DeleteFormation(deployment.AppID, deployment.NewReleaseID); e != nil {
				return
			}
			if e = client.SetAppRelease(deployment.AppID, deployment.NewReleaseID); e != nil {
				return
			}
		}
		e = nil
	}()
	if err := strategyFunc(client, deployment, events); err != nil {
		// TODO: log/handle error
		return nil
	}
	if err := client.SetAppRelease(deployment.AppID, deployment.NewReleaseID); err != nil {
		return err
	}
	if err := setDeploymentDone(deployment.ID); err != nil {
		return err
	}
	// signal success
	if err := sendDeploymentEvent(ct.DeploymentEvent{
		DeploymentID: deployment.ID,
		ReleaseID:    deployment.NewReleaseID,
		Status:       "complete",
	}); err != nil {
		log.Print(err)
	}
	return nil
}

func setDeploymentDone(id string) error {
	return db.Exec("UPDATE deployments SET finished_at = now() WHERE deployment_id = $1", id)
}

func sendDeploymentEvent(e ct.DeploymentEvent) error {
	if e.Status == "" {
		e.Status = "running"
	}
	query := "INSERT INTO deployment_events (deployment_id, release_id, job_type, job_state, status) VALUES ($1, $2, $3, $4, $5)"
	return db.Exec(query, e.DeploymentID, e.ReleaseID, e.JobType, e.JobState, e.Status)
}
