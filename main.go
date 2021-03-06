package main

// (c) Kailash Nadh, 2018. https://nadh.in
// MIT License.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	machinery "github.com/RichardKnop/machinery/v1"
	"github.com/RichardKnop/machinery/v1/config"
	mlog "github.com/RichardKnop/machinery/v1/log"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/knadh/sql-jobber/backends"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	// MySQL and Postgres drivers.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

const (
	dbMySQL    = "mysql"
	dbPostgres = "postgres"

	buildVersion = "unknown"
	buildDate    = "unknown"
)

type constants struct {
	ResultsDB string
}

type taskFunc func(jobID string, taskName string, ttl int, args ...interface{}) (int64, error)

// Jobber represents a collection of the tooling required to run a job server.
type Jobber struct {
	Tasks          Tasks
	Machinery      *machinery.Server
	Worker         *machinery.Worker
	DBs            DBs
	ResultBackends ResultBackends

	Logger *log.Logger
}

// DBConfig represents an SQL database's configuration.
type DBConfig struct {
	Type           string        `mapstructure:"type"`
	DSN            string        `mapstructure:"dsn"`
	MaxIdleConns   int           `mapstructure:"max_idle"`
	MaxActiveConns int           `mapstructure:"max_active"`
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
}

var (
	sysLog = log.New(os.Stdout, "JOBBER: ", log.Ldate|log.Ltime|log.Lshortfile)

	// Global Jobber container.
	jobber = &Jobber{
		Tasks:          make(Tasks),
		DBs:            make(DBs),
		ResultBackends: make(ResultBackends),
		Logger:         sysLog,
	}
)

func init() {
	// Command line flags.
	flagSet := flag.NewFlagSet("config", flag.ContinueOnError)
	flagSet.Usage = func() {
		sysLog.Println("SQL Jobber")
		sysLog.Println(flagSet.FlagUsages())
		os.Exit(0)
	}

	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.SetDefault("config", "config.toml")
	viper.SetDefault("server", ":6060")
	viper.SetDefault("sql-directory", []string{"./sql"})
	viper.SetDefault("worker-name", "sqljobber")
	viper.SetDefault("worker-concurrency", 10)
	viper.SetDefault("worker-only", false)

	flagSet.String("config", "config.toml", "Path to the TOML configuration file")
	flagSet.String("server", "127.0.0.1:6060", "Web server address")
	flagSet.StringSlice("sql-directory", []string{"./sql"}, "Path to directory with .sql scripts. Can be specified multiple times")
	flagSet.String("queue", "default_queue", "Name of the job queue to accept jobs from")
	flagSet.String("worker-name", "sqljobber", "Name of this worker instance")
	flagSet.Int("worker-concurrency", 10, "Number of concurrent worker threads to run")
	flagSet.Bool("worker-only", false, "Don't start the HTTP server and run in worker-only mode?")
	flagSet.Bool("version", false, "Current version of the build")

	flagSet.Parse(os.Args[1:])
	viper.BindPFlags(flagSet)

	// Config file.
	viper.SetConfigFile(viper.GetString("config"))
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatalf("error reading config: %s", err)
	}

	// Override Machinery's default logger.
	mlog.Set(log.New(os.Stdout, "MACHIN: ", log.Ldate|log.Ltime|log.Lshortfile))
}

func main() {
	// Display version.
	if viper.GetBool("version") {
		sysLog.Printf("commit: %v\nBuild: %v", buildVersion, buildDate)
		return
	}

	mode := "default"
	if viper.GetBool("worker-only") {
		mode = "worker only"
	}
	sysLog.Printf("starting server %s (queue = %s) in %s mode",
		viper.GetString("worker-name"),
		viper.GetString("queue"),
		mode)

	// Source and result backend DBs.
	var (
		dbs    map[string]DBConfig
		resDBs map[string]DBConfig
	)
	viper.UnmarshalKey("db", &dbs)
	viper.UnmarshalKey("results", &resDBs)

	// There should be at least one DB.
	if len(dbs) == 0 {
		sysLog.Fatal("found 0 source databases in config")
	}
	if len(resDBs) == 0 {
		sysLog.Fatal("found 0 result backends in config")
	}

	// Connect to source DBs.
	for dbName, cfg := range dbs {
		sysLog.Printf("connecting to source %s DB %s", cfg.Type, dbName)
		conn, err := connectDB(cfg)
		if err != nil {
			log.Fatal(err)
		}

		jobber.DBs[dbName] = conn
	}

	// Connect to backend DBs.
	for dbName, cfg := range resDBs {
		sysLog.Printf("connecting to result backend %s DB %s", cfg.Type, dbName)
		conn, err := connectDB(cfg)
		if err != nil {
			log.Fatal(err)
		}

		// Create a new backend instance.
		backend, err := backends.NewSQLBackend(conn,
			cfg.Type,
			viper.GetString(fmt.Sprintf("results.%s.results_table", dbName)),
			sysLog)
		if err != nil {
			log.Fatalf("error initializing result backend: %v", err)
		}

		jobber.ResultBackends[dbName] = backend
	}

	// Parse and load SQL queries.
	for _, d := range viper.GetStringSlice("sql-directory") {
		sysLog.Printf("loading SQL queries from directory: %s", d)
		tasks, err := loadSQLTasks(d, jobber.DBs, jobber.ResultBackends, viper.GetString("queue"))
		if err != nil {
			sysLog.Fatal(err)
		}

		for t, q := range tasks {
			if _, ok := jobber.Tasks[t]; ok {
				sysLog.Fatalf("duplicate task %s", t)
			}

			jobber.Tasks[t] = q
		}
		sysLog.Printf("loaded %d SQL queries from %s", len(tasks), d)
	}
	sysLog.Printf("loaded %d tasks in total", len(jobber.Tasks))

	// Bind the server HTTP endpoints.
	r := chi.NewRouter()
	r.Use(middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger: log.New(os.Stdout, "HTTP: ", log.Ldate|log.Ltime)}))

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		sendResponse(w, "welcome!")
	})
	r.Get("/tasks", handleGetTasksList)
	r.Post("/tasks/{taskName}/jobs", handlePostJob)

	r.Get("/jobs/{jobID}", handleGetJobStatus)
	r.Get("/jobs/queue/{queue}", handleGetPendingJobs)
	r.Delete("/jobs/{jobID}", handleDeleteJob)

	r.Post("/groups", handlePostJobGroup)
	r.Get("/groups/{groupID}", handleGetGroupStatus)

	// Setup the job server.
	var err error
	jobber.Machinery, err = connectJobServer(jobber, &config.Config{
		Broker:          viper.GetString("machinery.broker_address"),
		DefaultQueue:    viper.GetString("queue"),
		ResultBackend:   viper.GetString("machinery.state_address"),
		ResultsExpireIn: viper.GetInt("result_backend.results_ttl"),
	}, jobber.Tasks)
	if err != nil {
		log.Fatal(err)
	}

	// Start the HTTP server.
	if !viper.GetBool("worker-only") {
		sysLog.Printf("starting HTTP server on %s", viper.GetString("server"))
		go func() {
			sysLog.Println(http.ListenAndServe(viper.GetString("server"), r))
			os.Exit(0)
		}()
	}

	jobber.Worker = jobber.Machinery.NewWorker(viper.GetString("worker-name"),
		viper.GetInt("worker-concurrency"))
	jobber.Worker.Launch()
}
