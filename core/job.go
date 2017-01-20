package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/peakgames/s5cmd/op"
	"github.com/peakgames/s5cmd/opt"
	"github.com/peakgames/s5cmd/stats"
	"github.com/peakgames/s5cmd/url"
	"github.com/termie/go-shutil"
)

const dateFormat = "2006/01/02 15:04:05"

// JobArgument is an argument of the job. Can be a file/directory, an s3 url ("s3" is set in this case) or an arbitrary string.
type JobArgument struct {
	arg string
	s3  *url.S3Url
}

// Job is our basic job type.
type Job struct {
	sourceDesc         string // Source job description which we parsed this from
	command            string // Different from operation, as multiple commands can map to the same op
	operation          op.Operation
	args               []*JobArgument
	opts               opt.OptionList
	successCommand     *Job       // Next job to run if this one is successful
	failCommand        *Job       // .. if unsuccessful
	notifyChan         *chan bool // Ptr to chan to notify the job's success or fail
	isSubJob           bool
	numSuccess         *uint32 // Number of affected objects (only on batch operations)
	numFails           *uint32
	numAcceptableFails *uint32
}

// String formats the job using its command and arguments.
func (j Job) String() (s string) {
	s = j.command
	for _, a := range j.args {
		s += " " + a.arg
	}
	//s += " # from " + j.sourceDesc
	return
}

// MakeSubJob creates a sub-job linked to the original. sourceDesc is copied, numSuccess/numFails are linked. Returns a pointer to the new job.
func (j Job) MakeSubJob(command string, operation op.Operation, args []*JobArgument, opts opt.OptionList) *Job {
	ptr := args
	return &Job{
		sourceDesc:         j.sourceDesc,
		command:            command,
		operation:          operation,
		args:               ptr,
		opts:               opts,
		isSubJob:           true,
		numSuccess:         j.numSuccess,
		numFails:           j.numFails,
		numAcceptableFails: j.numAcceptableFails,
	}
}

// Clone duplicates a JobArgument and returns a pointer to a new one
func (a JobArgument) Clone() *JobArgument {
	var s url.S3Url
	if a.s3 != nil {
		s = a.s3.Clone()
	}
	return &JobArgument{a.arg, &s}
}

// Append appends a string to a JobArgument and returns itself.
func (a *JobArgument) Append(s string, isS3path bool) *JobArgument {
	if a.s3 != nil && !isS3path {
		// a is an S3 object but s is not
		s = strings.Replace(s, string(filepath.Separator), "/", -1)
	}
	if a.s3 == nil && isS3path {
		// a is a not an S3 object but s is
		s = strings.Replace(s, "/", string(filepath.Separator), -1)
	}

	a.arg += s
	if a.s3 != nil {
		a.s3.Key += s
	}
	return a
}

func (j *Job) out(short shortCode, format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	fmt.Println("                   ", short, s)
	if j.numSuccess != nil && short == shortOk {
		atomic.AddUint32(j.numSuccess, 1)
	}
	if j.numAcceptableFails != nil && short == shortOkWithError {
		atomic.AddUint32(j.numAcceptableFails, 1)
	}
	if j.numFails != nil && short == shortErr {
		atomic.AddUint32(j.numFails, 1)
	}
}

// PrintOK notifies the user about the positive outcome of the job. Internal operations are not shown, sub-jobs use short syntax.
func (j *Job) PrintOK(err AcceptableError) {
	if j.operation.IsInternal() {
		return
	}

	if j.isSubJob {
		if err != nil {
			j.out(shortOkWithError, `"%s" (%s)`, j, err.Error())
		} else {
			j.out(shortOk, `"%s"`, j)
		}
		return
	}

	errStr := ""
	okStr := "OK"
	if err != nil {
		errStr = " (" + err.Error() + ")"
		okStr = "OK?"
	}

	// Add successful jobs and considered-successful (finished with AcceptableError) jobs together
	var totalSuccess uint32 = 0
	if j.numSuccess != nil {
		totalSuccess += *j.numSuccess
	}
	if j.numAcceptableFails != nil {
		totalSuccess += *j.numAcceptableFails
		if *j.numAcceptableFails > 0 {
			okStr = "OK?"
		}
	}

	if totalSuccess > 0 {
		if j.numFails != nil && *j.numFails > 0 {
			log.Printf(`+%s "%s"%s (%d, %d failed)`, okStr, j, errStr, totalSuccess, *j.numFails)
		} else {
			log.Printf(`+%s "%s"%s (%d)`, okStr, j, errStr, totalSuccess)
		}
	} else if j.numFails != nil && *j.numFails > 0 {
		log.Printf(`+%s "%s"%s (%d failed)`, okStr, j, errStr, *j.numFails)
	} else {
		log.Printf(`+%s "%s"%s`, okStr, j, errStr)
	}
}

func (j *Job) PrintErr(err error) {
	if j.operation.IsInternal() {
		// TODO are we sure about ignoring errors from internal jobs?
		return
	}

	errStr := CleanupError(err)

	if j.isSubJob {
		j.out(shortErr, `"%s": %s`, j, errStr)
	} else {
		log.Printf(`-ERR "%s": %s`, j, errStr)
	}
}

// Notify informs the job's notify chan if the job failed or succeeded.
func (j *Job) Notify(ctx context.Context, err error) {
	if j.notifyChan == nil {
		return
	}
	res := err == nil

	for {
		select {
		case *j.notifyChan <- res:
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		j.out(shortInfo, "Waiting to notify %s", j.String())
		time.Sleep(time.Second)
	}
}

var (
	// ErrFileExistsButOk is used when a destination file already exists and opt.IfNotExists is set.
	ErrFileExistsButOk = NewAcceptableError("File already exists")
	// ErrS3ExistsButOk is used when a destination object already exists and opt.IfNotExists is set.
	ErrS3ExistsButOk = NewAcceptableError("Object already exists")
)

// Run runs the Job and returns error
func (j *Job) Run(wp *WorkerParams) error {
	//log.Printf("Running %v", j)

	doesFileExist := func(filename string) error {
		_, err := os.Stat(filename)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return ErrFileExistsButOk
	}

	switch j.operation {

	// Local operations
	case op.LocalDelete:
		return wp.st.IncrementIfSuccess(stats.FileOp, os.Remove(j.args[0].arg))

	case op.LocalCopy:
		var err error
		if j.opts.Has(opt.IfNotExists) {
			err = doesFileExist(j.args[1].arg)
			if err != nil {
				return err
			}
		}

		if j.opts.Has(opt.DeleteSource) {
			err = os.Rename(j.args[0].arg, j.args[1].arg)
		} else {
			_, err = shutil.Copy(j.args[0].arg, j.args[1].arg, true)
		}
		wp.st.IncrementIfSuccess(stats.FileOp, err)
		return err

	case op.ShellExec:
		strArgs := make([]string, 0)

		for i, a := range j.args {
			if i == 0 {
				continue
			}
			strArgs = append(strArgs, a.arg)
		}
		cmd := exec.CommandContext(wp.ctx, j.args[0].arg, strArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return wp.st.IncrementIfSuccess(stats.ShellOp, cmd.Run())

	// S3 operations
	case op.Copy:
		var err error
		if j.opts.Has(opt.IfNotExists) {
			_, err = s3head(wp.s3svc, j.args[1].s3)
			if err == nil {
				wp.st.IncrementIfSuccess(stats.S3Op, err)
				return ErrS3ExistsButOk
			}
		}

		var cls string

		if j.opts.Has(opt.RR) {
			cls = s3.ObjectStorageClassReducedRedundancy
		} else if j.opts.Has(opt.IA) {
			cls = s3.TransitionStorageClassStandardIa
		} else {
			cls = s3.ObjectStorageClassStandard
		}

		_, err = wp.s3svc.CopyObject(&s3.CopyObjectInput{
			Bucket:       aws.String(j.args[1].s3.Bucket),
			Key:          aws.String(j.args[1].s3.Key),
			CopySource:   aws.String(j.args[0].s3.Format()),
			StorageClass: aws.String(cls),
		})
		wp.st.IncrementIfSuccess(stats.S3Op, err)

		if j.opts.Has(opt.DeleteSource) && err == nil {
			_, err = s3delete(wp.s3svc, j.args[0].s3)
			wp.st.IncrementIfSuccess(stats.S3Op, err)
			// FIXME if err != nil try to rollback by deleting j.args[1].s3 ? What if we don't have permission to delete?
		}

		return err

	case op.BatchLocalCopy:
		subCmd := "cp"
		if j.opts.Has(opt.DeleteSource) {
			subCmd = "mv"
		}
		subCmd += j.opts.GetParams()

		st, err := os.Stat(j.args[0].arg)
		walkMode := err == nil && st.IsDir() // walk or glob?

		trimPrefix := j.args[0].arg
		globStart := j.args[0].arg
		if !walkMode {
			loc := strings.IndexAny(trimPrefix, GlobCharacters)
			if loc < 0 {
				return fmt.Errorf("Internal error, not a glob: %s", trimPrefix)
			}
			trimPrefix = trimPrefix[:loc]
		} else {
			if !strings.HasSuffix(globStart, string(filepath.Separator)) {
				globStart += string(filepath.Separator)
			}
			globStart = globStart + "*"
		}
		trimPrefix = path.Dir(trimPrefix)
		if trimPrefix == "." {
			trimPrefix = ""
		} else {
			trimPrefix += string(filepath.Separator)
		}

		recurse := j.opts.Has(opt.Recursive)

		err = wildOperation(wp, func(ch chan<- interface{}) error {
			defer func() {
				ch <- nil // send EOF
			}()

			// lister
			ma, err := filepath.Glob(globStart)
			if err != nil {
				return err
			}
			if len(ma) == 0 {
				if walkMode {
					return nil // Directory empty
				} else {
					return errors.New("Could not find match for glob")
				}
			}

			for _, f := range ma {
				s := f // copy
				st, _ := os.Stat(s)
				if !st.IsDir() {
					ch <- &s
				} else if recurse {
					err = filepath.Walk(s, func(path string, st os.FileInfo, err error) error {
						if err != nil {
							return err
						}
						if st.IsDir() {
							return nil
						}
						ch <- &path
						return nil
					})
					if err != nil {
						return err
					}
				}
			}
			return nil
		}, func(data interface{}) *Job {
			// callback
			if data == nil {
				return nil
			}
			fn := data.(*string)

			var dstFn string
			if j.opts.Has(opt.Parents) {
				dstFn = *fn
				if strings.Index(dstFn, trimPrefix) == 0 {
					dstFn = dstFn[len(trimPrefix):]
				}
			} else {
				dstFn = filepath.Base(*fn)
			}

			arg1 := JobArgument{
				*fn,
				nil,
			}
			arg2 := j.args[1].Clone().Append(dstFn, false)

			dir := filepath.Dir(arg2.arg)
			os.MkdirAll(dir, os.ModePerm)

			return j.MakeSubJob(subCmd, op.LocalCopy, []*JobArgument{&arg1, arg2}, j.opts)
		})

		return wp.st.IncrementIfSuccess(stats.FileOp, err)

	case op.Delete:
		_, err := s3delete(wp.s3svc, j.args[0].s3)
		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.BatchDelete:
		var jobArgs []*JobArgument
		srcBucket := *j.args[0].Clone()
		srcBucket.arg = "s3://" + srcBucket.s3.Bucket

		maxArgs := 1000

		initArgs := func() {
			jobArgs = make([]*JobArgument, 0, maxArgs+1)
			jobArgs = append(jobArgs, &srcBucket)
		}

		addArg := func(key *string) *Job {
			var subJob *Job

			if jobArgs == nil {
				initArgs()
			}

			if (key == nil || len(jobArgs) == maxArgs) && len(jobArgs) > 1 {
				subJob = j.MakeSubJob("batch-rm", op.BatchDeleteActual, jobArgs, opt.OptionList{})
				initArgs()
			}

			if key != nil {
				jobArgs = append(jobArgs, &JobArgument{arg: *key})
			}

			return subJob
		}

		err := s3wildOperation(j.args[0].s3, wp, func(li *s3listItem) *Job {
			if li == nil {
				return addArg(nil)
			}

			if li.isCommonPrefix {
				return nil
			}

			return addArg(li.key)
		})

		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.BatchDeleteActual:
		obj := make([]*s3.ObjectIdentifier, len(j.args)-1)
		for i, a := range j.args {
			if i == 0 {
				continue
			}
			obj[i-1] = &s3.ObjectIdentifier{Key: aws.String(a.arg)}
		}
		o, err := wp.s3svc.DeleteObjects(&s3.DeleteObjectsInput{
			Bucket: aws.String(j.args[0].s3.Bucket),
			Delete: &s3.Delete{
				Objects: obj,
			},
		})
		for _, o := range o.Deleted {
			j.out(shortOk, `Batch-delete s3://%s/%s`, j.args[0].s3.Bucket, *o.Key)
		}
		for _, e := range o.Errors {
			j.out(shortErr, `Batch-delete s3://%s/%s: %s`, j.args[0].s3.Bucket, *e.Key, *e.Message)
			if err != nil {
				err = errors.New(*e.Message)
			}
		}
		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.BatchDownload:
		subCmd := "cp"
		if j.opts.Has(opt.DeleteSource) {
			subCmd = "mv"
		}
		subCmd += j.opts.GetParams()

		err := s3wildOperation(j.args[0].s3, wp, func(li *s3listItem) *Job {
			if li == nil || li.isCommonPrefix {
				return nil
			}

			arg1 := JobArgument{
				"s3://" + j.args[0].s3.Bucket + "/" + *li.key,
				&url.S3Url{Bucket: j.args[0].s3.Bucket, Key: *li.key},
			}

			var dstFn string
			if j.opts.Has(opt.Parents) {
				dstFn = li.parsedKey
			} else {
				dstFn = path.Base(li.parsedKey)
			}

			arg2 := j.args[1].Clone().Append(dstFn, true)
			subJob := j.MakeSubJob(subCmd, op.Download, []*JobArgument{&arg1, arg2}, j.opts)
			if *li.class == s3.ObjectStorageClassGlacier {
				subJob.out(shortErr, `"%s": Cannot download glacier object`, arg1.arg)
				return nil
			}
			dir := filepath.Dir(arg2.arg)
			os.MkdirAll(dir, os.ModePerm)
			return subJob
		})

		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.BatchUpload:
		subCmd := "cp"
		if j.opts.Has(opt.DeleteSource) {
			subCmd = "mv"
		}
		subCmd += j.opts.GetParams()

		st, err := os.Stat(j.args[0].arg)
		walkMode := err == nil && st.IsDir() // walk or glob?

		trimPrefix := j.args[0].arg
		if !walkMode {
			loc := strings.IndexAny(trimPrefix, GlobCharacters)
			if loc < 0 {
				return fmt.Errorf("Internal error, not a glob: %s", trimPrefix)
			}
			trimPrefix = trimPrefix[:loc]
		}
		trimPrefix = path.Dir(trimPrefix)
		if trimPrefix == "." {
			trimPrefix = ""
		} else {
			trimPrefix += string(filepath.Separator)
		}

		err = wildOperation(wp, func(ch chan<- interface{}) error {
			defer func() {
				ch <- nil // send EOF
			}()
			// lister
			if walkMode {
				err := filepath.Walk(j.args[0].arg, func(path string, st os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if st.IsDir() {
						return nil
					}
					ch <- &path
					return nil
				})
				return err
			} else {
				ma, err := filepath.Glob(j.args[0].arg)
				if err != nil {
					return err
				}
				if len(ma) == 0 {
					return errors.New("Could not find match for glob")
				}

				for _, f := range ma {
					s := f // copy
					st, _ = os.Stat(s)
					if !st.IsDir() {
						ch <- &s
					}
				}
				return nil
			}
		}, func(data interface{}) *Job {
			// callback
			if data == nil {
				return nil
			}
			fn := data.(*string)

			var dstFn string
			if j.opts.Has(opt.Parents) {
				dstFn = *fn
				if strings.Index(dstFn, trimPrefix) == 0 {
					dstFn = dstFn[len(trimPrefix):]
				}
			} else {
				dstFn = filepath.Base(*fn)
			}

			arg1 := JobArgument{
				*fn,
				nil,
			}
			arg2 := j.args[1].Clone().Append(dstFn, false)
			return j.MakeSubJob(subCmd, op.Upload, []*JobArgument{&arg1, arg2}, j.opts)
		})

		return wp.st.IncrementIfSuccess(stats.FileOp, err)

	case op.Download:
		srcFn := path.Base(j.args[0].arg)
		destFn := j.args[1].arg

		if j.opts.Has(opt.IfNotExists) {
			err := doesFileExist(destFn)
			if err != nil {
				return err
			}
		}

		f, err := os.Create(destFn)
		if err != nil {
			return err
		}

		j.out(shortInfo, "Downloading %s...", srcFn)

		ch := make(chan error, 1)

		go func() {
			_, err := wp.s3dl.Download(f, &s3.GetObjectInput{
				Bucket: aws.String(j.args[0].s3.Bucket),
				Key:    aws.String(j.args[0].s3.Key),
			})
			ch <- err
			close(ch)
		}()

		select {
		case <-wp.ctx.Done():
			err = ErrInterrupted
		case err = <-ch:
			break
		}

		f.Close() // Race: s3dl.Download or us?

		wp.st.IncrementIfSuccess(stats.S3Op, err)
		if err != nil {
			os.Remove(destFn) // Remove partly downloaded file
		} else if j.opts.Has(opt.DeleteSource) {
			_, err = s3delete(wp.s3svc, j.args[0].s3)
			wp.st.IncrementIfSuccess(stats.S3Op, err)
		}

		return err

	case op.Upload:
		const bytesInMb = float64(1024 * 1024)

		srcFn := filepath.Base(j.args[0].arg)
		s, err := os.Stat(j.args[0].arg)
		if err != nil {
			return err
		}

		if j.opts.Has(opt.IfNotExists) {
			_, err = s3head(wp.s3svc, j.args[1].s3)
			if err == nil {
				wp.st.IncrementIfSuccess(stats.S3Op, err)
				return ErrS3ExistsButOk
			}
		}

		f, err := os.Open(j.args[0].arg)
		if err != nil {
			return err
		}

		defer f.Close()

		filesize := s.Size()

		numPartsNeeded := filesize / wp.poolParams.ChunkSizeBytes
		chunkSize := int64(wp.poolParams.ChunkSizeBytes / int64(bytesInMb))
		if numPartsNeeded > s3manager.MaxUploadParts {
			cSize := float64(filesize / s3manager.MaxUploadParts)
			chunkSize = int64(math.Ceil(cSize / bytesInMb))
			j.out(shortInfo, "Uploading %s... (%d bytes) (chunk size %d MB)", srcFn, filesize, chunkSize)
		} else {
			j.out(shortInfo, "Uploading %s... (%d bytes)", srcFn, filesize)
		}

		ch := make(chan error, 1)

		go func(chunkSizeInBytes int64) {
			var cls string

			if j.opts.Has(opt.RR) {
				cls = s3.ObjectStorageClassReducedRedundancy
			} else if j.opts.Has(opt.IA) {
				cls = s3.TransitionStorageClassStandardIa
			} else {
				cls = s3.ObjectStorageClassStandard
			}

			_, err := wp.s3ul.Upload(&s3manager.UploadInput{
				Bucket:       aws.String(j.args[1].s3.Bucket),
				Key:          aws.String(j.args[1].s3.Key),
				Body:         f,
				StorageClass: aws.String(cls),
			}, func(u *s3manager.Uploader) {
				u.PartSize = chunkSizeInBytes
			})

			ch <- err
			close(ch)
		}(chunkSize * int64(bytesInMb))

		select {
		case <-wp.ctx.Done():
			err = ErrInterrupted
		case err = <-ch:
			break
		}

		f.Close()

		wp.st.IncrementIfSuccess(stats.S3Op, err)
		if j.opts.Has(opt.DeleteSource) && err == nil {
			err = wp.st.IncrementIfSuccess(stats.FileOp, os.Remove(j.args[0].arg))
		}
		return err

	case op.ListBuckets:
		o, err := wp.s3svc.ListBuckets(&s3.ListBucketsInput{})
		if err == nil {
			for _, b := range o.Buckets {
				j.out(shortOk, "%s  s3://%s", b.CreationDate.Format(dateFormat), *b.Name)
			}
		}
		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.List:
		err := s3wildOperation(j.args[0].s3, wp, func(li *s3listItem) *Job {
			if li == nil {
				return nil
			}

			if li.isCommonPrefix {
				j.out(shortOk, "%19s %1s  %12s  %s", "", "", "DIR", li.parsedKey)
			} else {
				var cls string

				switch *li.class {
				case s3.ObjectStorageClassStandard:
					cls = ""
				case s3.ObjectStorageClassGlacier:
					cls = "G"
				case s3.ObjectStorageClassReducedRedundancy:
					cls = "R"
				case s3.TransitionStorageClassStandardIa:
					cls = "I"
				default:
					cls = "?"
				}
				j.out(shortOk, "%s %1s  %12d  %s", li.lastModified.Format(dateFormat), cls, li.size, li.parsedKey)
			}

			return nil
		})

		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.Size:
		var size, count int64
		err := s3wildOperation(j.args[0].s3, wp, func(li *s3listItem) *Job {
			if li == nil || li.isCommonPrefix {
				return nil
			}
			size += li.size
			count++
			return nil
		})
		if err == nil {
			j.out(shortOk, "%d bytes in %d objects: %s", size, count, j.args[0].s3)
		}
		return wp.st.IncrementIfSuccess(stats.S3Op, err)

	case op.Abort:
		var (
			exitCode int64 = -1
			err      error
		)

		if len(j.args) > 0 {
			exitCode, err = strconv.ParseInt(j.args[0].arg, 10, 8)
			if err != nil {
				exitCode = 255
			}
		}

		ef := wp.ctx.Value(ExitFuncKey).(func(int))
		ef(int(exitCode))

		return nil

	// Unhandled
	default:
		return fmt.Errorf("Unhandled operation %v", j.operation)
	}

}

type wildLister func(chan<- interface{}) error
type wildCallback func(interface{}) *Job

/*
wildOperation is the cornerstone of sub-job launching.

It will run lister() when ready and expect data from ch. On EOF, a single nil should be passed into ch.
Data received from ch will be passed to callback() which in turn will create a *Job entry (or nil for no job)
Then this entry is submitted to the subJobQueue chan.

After lister() completes, the sub-jobs are tracked
The fn will return when all jobs are processed, and it will return with error if even a single sub-job was not successful

Midway-failing lister() fns are not thoroughly tested and may hang or panic
*/

func wildOperation(wp *WorkerParams, lister wildLister, callback wildCallback) error {
	ch := make(chan interface{})
	closer := make(chan bool)
	notifyChan := make(chan bool)
	var subJobCounter uint32 // number of total subJobs issued

	// This goroutine will read ls results from ch and issue new subJobs
	go func() {
		defer close(closer) // Close closer when goroutine exits
		for {
			select {
			case data, ok := <-ch:
				if !ok {
					// Channel closed early: err returned from s3list?
					return
				}
				j := callback(data)
				if j != nil {
					j.notifyChan = &notifyChan
					subJobCounter++
					*wp.subJobQueue <- j
				}
				if data == nil {
					// End of listing
					return
				}
			}
		}
	}()

	var (
		successfulSubJobs uint32
		processedSubJobs  uint32
	)
	// This goroutine will tally successful and total processed sub-jobs
	go func() {
		for {
			select {
			case res, ok := <-notifyChan:
				if !ok {
					return
				}
				atomic.AddUint32(&processedSubJobs, 1)
				if res {
					atomic.AddUint32(&successfulSubJobs, 1)
				}
			}
		}
	}()

	// Do the actual work
	err := lister(ch)
	if err == nil {
		// This select ensures that we don't return to the main loop without completely getting the list results (and queueing up operations on subJobQueue)
		<-closer // Wait for EOF on goroutine

		var p, s uint32
		for { // wait for all jobs to finish
			p = atomic.LoadUint32(&processedSubJobs)
			if p < subJobCounter {
				time.Sleep(time.Second)
			} else {
				break
			}
		}

		s = atomic.LoadUint32(&successfulSubJobs)
		if s != subJobCounter {
			err = fmt.Errorf("Not all jobs completed successfully: %d/%d", s, subJobCounter)
		}
	}
	close(ch)
	close(notifyChan)
	return err
}
