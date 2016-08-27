package remote

import (
	"sync"
	"time"

	"github.com/mongodb/amboy"
	"github.com/mongodb/amboy/registry"
	"github.com/pkg/errors"
	"github.com/tychoish/grip"
	"golang.org/x/net/context"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

// MongoDBQueueDriver is a type that represents and wraps a queues
// persistence of jobs *and* locks to a MongoDB instance.
type MongoDBQueueDriver struct {
	name            string
	mongodbURI      string
	dbName          string
	jobsCollection  *mgo.Collection
	locksCollection *mgo.Collection
	session         *mgo.Session
	canceler        context.CancelFunc
	locks           struct {
		cache map[string]*MongoDBJobLock
		mutex sync.RWMutex
	}
}

// NewMongoDBQueueDriver creates a driver object given a name, which
// serves as a prefix for collection names, and a MongoDB connection
func NewMongoDBQueueDriver(name, uri string) *MongoDBQueueDriver {
	d := &MongoDBQueueDriver{
		name:       name,
		dbName:     "amboy",
		mongodbURI: uri,
	}
	d.locks.cache = make(map[string]*MongoDBJobLock)

	return d
}

// Open creates a connection to MongoDB, and returns an error if
// there's a problem connecting.
func (d *MongoDBQueueDriver) Open(ctx context.Context) error {
	if d.session != nil {
		return nil
	}

	session, err := mgo.Dial(d.mongodbURI)
	if err != nil {
		return errors.Wrapf(err, "problem opening connection to mongodb at '%s", d.mongodbURI)
	}

	dCtx, cancel := context.WithCancel(ctx)
	d.canceler = cancel
	d.session = session

	err = d.setupDB(session)
	if err != nil {
		return errors.Wrap(err, "problem setting up database")
	}

	go func() {
		<-dCtx.Done()

		if d.session != nil {
			d.session.Close()
			d.session = nil
			grip.Info("closing session for mongodb driver")
		}
	}()

	return nil
}

func (d *MongoDBQueueDriver) setupDB(session *mgo.Session) error {
	d.jobsCollection = session.DB(d.dbName).C(d.name + ".jobs")
	d.locksCollection = session.DB(d.dbName).C(d.name + ".locks")
	return errors.Wrap(d.createIndexes(), "problem building indexes")
}

func (d *MongoDBQueueDriver) createIndexes() error {
	catcher := grip.NewCatcher()

	catcher.Add(d.jobsCollection.EnsureIndexKey("completed", "dispatched"))
	catcher.Add(d.locksCollection.EnsureIndexKey("locked"))

	grip.WarningWhen(catcher.HasErrors(), "problem creating indexes")
	grip.DebugWhen(!catcher.HasErrors(), "created indexes successfully")

	return catcher.Resolve()
}

// Close terminates the connection to the database server.
func (d *MongoDBQueueDriver) Close() {
	if d.canceler != nil {
		d.canceler()
	}
}

// Put saves a job object to the persistence layer, and returns an
// error from the MongoDB driver as needed.
func (d *MongoDBQueueDriver) Put(j amboy.Job) error {
	i, err := registry.MakeJobInterchange(j)
	if err != nil {
		return errors.Wrap(err, "problem converting job to interchange format")
	}

	err = d.jobsCollection.Insert(i)
	if err != nil {
		return errors.Wrap(err,
			"problem inserting document into collection during PUT")
	}

	return nil
}

// Get takes the name of a job and returns an amboy.Job object from
// the persistence layer for the job matching that unique id.
func (d *MongoDBQueueDriver) Get(name string) (amboy.Job, error) {
	j := &registry.JobInterchange{}
	err := d.jobsCollection.FindId(name).One(j)
	grip.Debugf("GET operation: [name='%s', payload='%+v' error='%v']", name, j, err)

	if err != nil {
		return nil, errors.Wrapf(err, "GET problem fetching '%s'", name)
	}

	output, err := registry.ConvertToJob(j)
	if err != nil {
		return nil, errors.Wrapf(err,
			"GET problem converting '%s' to job object", name)
	}

	return output, nil
}

// Reload takes am amboy.Job object and returns. This operation logs
// errors, but will return the original document if it encounters an
// error reloading a document.
func (d *MongoDBQueueDriver) Reload(j amboy.Job) amboy.Job {
	newJob, err := d.Get(j.ID())
	if err != nil {
		grip.Warningf("encountered error reloading job %s: %s",
			j.ID(), err.Error())
		return j
	}

	return newJob
}

// Save takes a job object and updates that job in the persistence
// layer. This operation is based on an update, and an existing job
// with the same "ID()" property must exist. Use "Put()" to insert a
// new job into the database.
func (d *MongoDBQueueDriver) Save(j amboy.Job) error {
	name := j.ID()
	job, err := registry.MakeJobInterchange(j)
	if err != nil {
		return errors.Wrap(err, "problem converting error to interchange format")
	}

	err = d.jobsCollection.UpdateId(name, job)
	if err != nil {
		return errors.Wrapf(err, "problem updating ")
	}

	grip.Debugf("saved job '%s'", name)

	return nil
}

// Jobs returns a channel containing all jobs persisted by this
// driver. This includes all completed, pending, and locked
// jobs. Errors, including those with connections to MongoDB or with
// corrupt job documents, are logged.
func (d *MongoDBQueueDriver) Jobs() <-chan amboy.Job {
	output := make(chan amboy.Job)
	go func() {
		defer close(output)

		results := d.jobsCollection.Find(nil).Iter()
		defer grip.CatchError(results.Close())
		j := &registry.JobInterchange{}
		for results.Next(j) {
			job, err := registry.ConvertToJob(j)
			if err != nil {
				grip.CatchError(err)
				continue
			}
			output <- job
		}
		grip.CatchError(results.Err())
	}()
	return output
}

// Next returns one job, not marked complete from the database.
func (d *MongoDBQueueDriver) Next() amboy.Job {
	j := &registry.JobInterchange{}

	err := d.jobsCollection.Find(bson.M{"completed": false, "dispatched": false}).One(j)
	if err != nil {
		grip.DebugWhenln(err.Error() != "not found",
			"could not find a job ready for processing:", err.Error())
		return nil
	}

	j.Dispatched = true
	err = d.jobsCollection.UpdateId(j.Name, j)
	if err != nil {
		grip.Errorf("problem marking job %s dispatched: %+v", j.Name, err.Error())
		return nil
	}

	job, err := registry.ConvertToJob(j)
	if err != nil {
		grip.Errorf("problem converting from MongoDB to job object: %+v", err.Error())
		return nil
	}

	return job
}

// Stats returns a Stats object that contains information about the
// state of the queue in the persistence layer. This operation
// performs a number of asynchronous queries to collect data, and in
// an active system with a number of active queues, stats may report
// incongruous data.
func (d *MongoDBQueueDriver) Stats() Stats {
	stats := Stats{}

	numJobs, err := d.jobsCollection.Count()
	grip.ErrorWhenf(err != nil,
		"problem getting count from jobs collection (%s): %+v ",
		d.jobsCollection, err)
	stats.Total = numJobs

	numIncomplete, err := d.jobsCollection.Find(bson.M{"completed": false}).Count()
	grip.ErrorWhenf(err != nil,
		"problem getting count of pending jobs (%s): %+v ",
		d.jobsCollection, err)
	stats.Pending = numIncomplete

	numLocked, err := d.locksCollection.Find(bson.M{"locked": true}).Count()
	grip.ErrorWhenf(err != nil,
		"problem getting count of locked Jobs (%s): %+v",
		d.jobsCollection, err)
	stats.Locked = numLocked

	// computed stats
	stats.Complete = stats.Total - stats.Pending
	stats.Unlocked = stats.Total - stats.Locked

	return stats
}

// GetLock takes the name of a job and returns, creating if necessary,
// if the job exists, a MongoDBJobLock instance for that job.
func (d *MongoDBQueueDriver) GetLock(ctx context.Context, job amboy.Job) (RemoteJobLock, error) {
	name := job.ID()
	start := time.Now()

	if ctx.Err() != nil {
		return nil, errors.New("job canceled before getting lock")
	}

	d.locks.mutex.Lock()
	defer d.locks.mutex.Unlock()

	lock, ok := d.locks.cache[name]
	if ok {
		grip.Debugf("found cached lock (%s): duration = %s", name, time.Since(start))
		return lock, nil
	}

	lock, err := NewMongoDBJobLock(ctx, name, d.locksCollection)
	if err != nil {
		return nil, err
	}

	d.locks.cache[name] = lock
	grip.Debugf("created new lock (%s): duration = %s", name, time.Since(start))
	return lock, nil
}
