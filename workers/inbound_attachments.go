package workers

import (
	"context"
	"errors"
	"log"
	"time"

	"stonesuite-notify/inbound"
)

const (
	inboundJobPollInterval = 3 * time.Second
	// One file at a time: a file is held whole in memory, and this service runs on
	// a 256 MB machine.
	inboundJobBatchSize = 1
	// A job left "processing" this long was abandoned (a crash, or the machine
	// stopping mid-copy) and is picked up again.
	inboundJobStaleAfter = 5 * time.Minute
	// inboundJobTimeout bounds one attempt: list, download and upload.
	inboundJobTimeout = 2 * time.Minute
)

// errAttachmentGone means Resend no longer lists the file for its mail.
var errAttachmentGone = errors.New("received attachment is no longer listed by the provider")

// InboundAttachmentWorker copies the files of received customer replies to
// StoneSuite-Backend, one queued job at a time. The webhook that received the
// mail only queued them (it has seconds to answer); this does the slow part, with
// retries. Resend's download links last about an hour, so every attempt asks for a
// fresh one rather than trusting a stored link.
type InboundAttachmentWorker struct {
	Jobs       inbound.JobStore
	Lister     inbound.AttachmentLister
	Downloader inbound.FileDownloader
	Uploader   inbound.FileUploader
}

// Run blocks, polling on a ticker until ctx is cancelled.
func (w InboundAttachmentWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(inboundJobPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

func (w InboundAttachmentWorker) pollOnce(ctx context.Context) {
	jobs, err := w.Jobs.ClaimDue(ctx, inboundJobBatchSize, inboundJobStaleAfter)
	if err != nil {
		log.Printf("workers: inbound attachment claim: %v", err)
		return
	}
	for _, job := range jobs {
		w.handle(ctx, job)
	}
}

// handle runs one attempt and settles the job by how it went: done, never going
// to work (kept, with the reason, for whoever looks into it), or worth another
// try after a backoff.
func (w InboundAttachmentWorker) handle(ctx context.Context, job inbound.Job) {
	attemptCtx, cancel := context.WithTimeout(ctx, inboundJobTimeout)
	defer cancel()

	err := w.copyFile(attemptCtx, job)
	switch {
	case err == nil:
		w.settle("complete", job, w.Jobs.Complete(ctx, job.ID))
	case errors.Is(err, errAttachmentGone), errors.Is(err, inbound.ErrFileTooLarge):
		// Nothing more can be done for this file; the conversation shows it as
		// unavailable once its copy has stayed pending long enough.
		log.Printf("workers: inbound attachment %s of %s dropped: %v", job.AttachmentID, job.EmailID, err)
		w.settle("complete", job, w.Jobs.Complete(ctx, job.ID))
	case inbound.IsRetryable(err):
		log.Printf("workers: inbound attachment %s of %s, attempt %d: %v", job.AttachmentID, job.EmailID, job.Attempts+1, err)
		w.settle("retry", job, w.Jobs.Retry(ctx, job.ID, job.Attempts+1, err.Error()))
	default:
		log.Printf("workers: inbound attachment %s of %s gave up: %v", job.AttachmentID, job.EmailID, err)
		w.settle("fail", job, w.Jobs.Fail(ctx, job.ID, err.Error()))
	}
}

// settle logs a failure to record a job's outcome. The job is left as claimed and
// is picked up again once it has been "processing" long enough.
func (w InboundAttachmentWorker) settle(action string, job inbound.Job, err error) {
	if err != nil {
		log.Printf("workers: inbound attachment job %s: %s: %v", job.ID, action, err)
	}
}

// copyFile lists the mail's files for a fresh link, downloads the one the job is
// for, and uploads it to the backend.
func (w InboundAttachmentWorker) copyFile(ctx context.Context, job inbound.Job) error {
	files, err := w.Lister.ListAttachments(ctx, job.EmailID)
	if err != nil {
		return err
	}
	var file *inbound.ReceivedAttachment
	for i := range files {
		if files[i].ID == job.AttachmentID {
			file = &files[i]
			break
		}
	}
	if file == nil {
		return errAttachmentGone
	}
	if file.Size > inbound.MaxFileBytes {
		return inbound.ErrFileTooLarge
	}
	data, err := w.Downloader.Download(ctx, file.DownloadURL, inbound.MaxFileBytes)
	if err != nil {
		return err
	}
	return w.Uploader.UploadAttachment(ctx, job.UploadRef, job.AttachmentID, file.ContentType, data)
}
