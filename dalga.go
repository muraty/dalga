package main

// TODO list
// seperate files
// make http server closeable
// write basic integration tests
// handle mysql disconnect
// handle rabbitmq disconnect

import (
	"code.google.com/p/gcfg"
	"database/sql"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/streadway/amqp"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	debugging  = flag.Bool("d", false, "turn on debug messages")
	configPath = flag.String("c", "", "config file path")
)

type Dalga struct {
	cfg          *Config
	db           *sql.DB
	rabbit       *amqp.Connection
	channel      *amqp.Channel
	newJobs      chan *Job
	canceledJobs chan *Job
}

func NewDalga(config *Config) *Dalga {
	return &Dalga{
		cfg:          config,
		newJobs:      make(chan *Job),
		canceledJobs: make(chan *Job),
	}
}

type Config struct {
	MySQL struct {
		User     string
		Password string
		Host     string
		Port     string
		Db       string
		Table    string
	}
	RabbitMQ struct {
		User     string
		Password string
		Host     string
		Port     string
		VHost    string
		Exchange string
	}
	HTTP struct {
		Host string
		Port string
	}
}

// NewConfig returns a pointer to a newly created Config initialized with default parameters.
func NewConfig() *Config {
	c := &Config{}
	c.MySQL.User = "root"
	c.MySQL.Host = "localhost"
	c.MySQL.Port = "3306"
	c.MySQL.Db = "test"
	c.MySQL.Table = "dalga"
	c.RabbitMQ.User = "guest"
	c.RabbitMQ.Password = "guest"
	c.RabbitMQ.Host = "localhost"
	c.RabbitMQ.Port = "5672"
	c.RabbitMQ.VHost = "/"
	c.HTTP.Host = "0.0.0.0"
	c.HTTP.Port = "17500"
	return c
}

type Job struct {
	RoutingKey string
	Body       string
	Interval   time.Duration
	NextRun    time.Time
}

func debug(args ...interface{}) {
	if *debugging {
		log.Println(args...)
	}
}

func (dalga *Dalga) makeHandler(fn func(http.ResponseWriter, *http.Request, *Dalga)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(w, r, dalga)
	}
}

// hadleSchedule is the web server endpoint for path: /schedule
func handleSchedule(w http.ResponseWriter, r *http.Request, d *Dalga) {
	routingKey, body, intervalString := r.FormValue("routing_key"), r.FormValue("body"), r.FormValue("interval")
	debug("/schedule", routingKey, body)

	intervalUint64, err := strconv.ParseUint(intervalString, 10, 32)
	if err != nil {
		http.Error(w, "Cannot parse interval", http.StatusBadRequest)
		return
	}

	if intervalUint64 < 1 {
		http.Error(w, "interval must be >= 1", http.StatusBadRequest)
		return
	}

	job := NewJob(routingKey, body, uint32(intervalUint64))
	err = job.Enter(d)
	if err != nil {
		panic(err)
	}

	// Wake up the publisher.
	//
	// publisher() may be sleeping for the next job on the queue
	// at the time we schedule a new Job. Let it wake up so it can
	// re-fetch the new Job from the front of the queue.
	//
	// The code below is an idiom for non-blocking send to a channel.
	select {
	case d.newJobs <- job:
		debug("Sent new job signal")
	default:
		debug("Did not send new job signal")
	}
}

// handleCancel is the web server endpoint for path: /cancel
func handleCancel(w http.ResponseWriter, r *http.Request, d *Dalga) {
	routingKey, body := r.FormValue("routing_key"), r.FormValue("body")
	debug("/cancel", routingKey, body)

	err := CancelJob(routingKey, body, d)
	if err != nil {
		panic(err)
	}

	select {
	case d.canceledJobs <- &Job{RoutingKey: routingKey, Body: body}:
		debug("Sent cancel signal")
	default:
		debug("Did not send cancel signal")
	}
}

// front returns the first job to be run in the queue.
func (d *Dalga) front() (*Job, error) {
	var interval uint
	j := Job{}
	row := d.db.QueryRow("SELECT routing_key, body, `interval`, next_run " +
		"FROM " + d.cfg.MySQL.Table + " " +
		"ORDER BY next_run ASC LIMIT 1")
	err := row.Scan(&j.RoutingKey, &j.Body, &interval, &j.NextRun)
	if err != nil {
		return nil, err
	}
	j.Interval = time.Duration(interval) * time.Second
	return &j, nil
}

// Publish sends a message to exchange defined in the config and
// updates the Job's next run time on the database.
func (j *Job) Publish(d *Dalga) error {
	debug("publish", *j)

	// Update next run time
	_, err := d.db.Exec("UPDATE "+d.cfg.MySQL.Table+" "+
		"SET next_run=? "+
		"WHERE routing_key=? AND body=?",
		time.Now().UTC().Add(j.Interval), j.RoutingKey, j.Body)
	if err != nil {
		return err
	}

	// Send a message to RabbitMQ
	err = d.channel.Publish(d.cfg.RabbitMQ.Exchange, j.RoutingKey, false, false, amqp.Publishing{
		Headers: amqp.Table{
			"interval":     j.Interval.Seconds(),
			"published_at": time.Now().UTC().String(),
		},
		ContentType:     "text/plain",
		ContentEncoding: "UTF-8",
		Body:            []byte(j.Body),
		DeliveryMode:    amqp.Persistent,
		Priority:        0,
		Expiration:      strconv.FormatUint(uint64(j.Interval.Seconds()), 10) + "000",
	})
	if err != nil {
		return err
	}

	return nil
}

func NewJob(routingKey string, body string, interval uint32) *Job {
	job := Job{
		RoutingKey: routingKey,
		Body:       body,
		Interval:   time.Duration(interval) * time.Second,
	}
	job.SetNewNextRun()
	return &job
}

// Remaining returns the duration until the job's next scheduled time.
func (j *Job) Remaining() time.Duration {
	return -time.Since(j.NextRun)
}

// SetNewNextRun calculates the new run time according to current time and sets it on the job.
func (j *Job) SetNewNextRun() {
	j.NextRun = time.Now().UTC().Add(j.Interval)
}

// Enter puts the job to the waiting queue.
func (j *Job) Enter(d *Dalga) error {
	interval := j.Interval.Seconds()
	_, err := d.db.Exec("INSERT INTO "+d.cfg.MySQL.Table+" "+
		"(routing_key, body, `interval`, next_run) "+
		"VALUES(?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE "+
		"next_run=DATE_ADD(next_run, INTERVAL (? - `interval`) SECOND), "+
		"`interval`=?",
		j.RoutingKey, j.Body, interval, j.NextRun, interval, interval)
	return err
}

// Cancel removes the job from the waiting queue.
func CancelJob(routingKey, body string, d *Dalga) error {
	_, err := d.db.Exec("DELETE FROM "+d.cfg.MySQL.Table+" "+
		"WHERE routing_key=? AND body=?", routingKey, body)
	return err
}

// publisher runs a loop that reads the next Job from the queue and publishes it.
func (d *Dalga) publisher() {
	publish := func(j *Job) {
		err := j.Publish(d)
		if err != nil {
			fmt.Println(err)
		}
	}

	for {
		debug("")

		job, err := d.front()
		if err != nil {
			if strings.Contains(err.Error(), "no rows in result set") {
				debug("No waiting jobs in the queue")
				debug("Waiting wakeup signal")
				job = <-d.newJobs
				debug("Got wakeup signal")
			} else {
				fmt.Println(err)
				continue
			}
		}

	CheckNextRun:
		remaining := job.Remaining()
		debug("Next job:", job, "Remaining:", remaining)

		now := time.Now().UTC()
		if job.NextRun.After(now) {
			// Wait until the next Job time or
			// the webserver's /schedule handler wakes us up
			debug("Sleeping for job:", remaining)
			select {
			case <-time.After(remaining):
				debug("Job sleep time finished")
				publish(job)
			case newJob := <-d.newJobs:
				debug("A new job has been scheduled")
				if newJob.NextRun.Before(job.NextRun) {
					debug("The new job comes before out current job")
					job = newJob // Process the new job next
				}
				// Continue processing the current job without fetching from database
				goto CheckNextRun
			case canceledJob := <-d.canceledJobs:
				debug("A job has been cancelled")
				if (job.RoutingKey == canceledJob.RoutingKey) && (job.Body == canceledJob.Body) {
					// The job we are waiting for has been canceled.
					// We need to fetch the next job in the queue.
					debug("The cancelled job is our current job")
					continue
				}
				// Continue to process our current job
				goto CheckNextRun
			}
		} else {
			publish(job)
		}

	}
}

func main() {
	var err error
	flag.Parse()

	// Read config
	cfg := NewConfig()
	if *configPath != "" {
		err = gcfg.ReadFileInto(cfg, *configPath)
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Println("Read config: ", cfg)
	}

	// Create new Dalga instance
	dalga := NewDalga(cfg)

	// Connect to database
	mysql := dalga.cfg.MySQL
	dsn := mysql.User + ":" + mysql.Password + "@" + "tcp(" + mysql.Host + ":" + mysql.Port + ")/" + mysql.Db + "?parseTime=true"
	dalga.db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	err = dalga.db.Ping()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Connected to MySQL")

	// Connect to RabbitMQ
	rabbit := dalga.cfg.RabbitMQ
	uri := "amqp://" + rabbit.User + ":" + rabbit.Password + "@" + rabbit.Host + ":" + rabbit.Port + rabbit.VHost
	dalga.rabbit, err = amqp.Dial(uri)
	if err != nil {
		log.Fatal(err)
	}
	dalga.channel, err = dalga.rabbit.Channel()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Connected to RabbitMQ")

	// Run publisher
	go dalga.publisher()

	// Start HTTP server
	addr := cfg.HTTP.Host + ":" + cfg.HTTP.Port
	http.HandleFunc("/schedule", dalga.makeHandler(handleSchedule))
	http.HandleFunc("/cancel", dalga.makeHandler(handleCancel))
	http.ListenAndServe(addr, nil)
}
