package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"stonesuite-notify/inbound"
)

type retriedJob struct {
	id       string
	attempts int
	message  string
}

type fakeJobStore struct {
	claim     []inbound.Job
	claimErr  error
	completed []string
	failed    []string
	retried   []retriedJob
}

func (f *fakeJobStore) Enqueue(context.Context, string, string, []string) error {
	return nil
}

func (f *fakeJobStore) ClaimDue(context.Context, int, time.Duration) ([]inbound.Job, error) {
	jobs := f.claim
	f.claim = nil
	return jobs, f.claimErr
}

func (f *fakeJobStore) Complete(_ context.Context, id string) error {
	f.completed = append(f.completed, id)
	return nil
}

func (f *fakeJobStore) Retry(_ context.Context, id string, attempts int, lastError string) error {
	f.retried = append(f.retried, retriedJob{id: id, attempts: attempts, message: lastError})
	return nil
}

func (f *fakeJobStore) Fail(_ context.Context, id string, _ string) error {
	f.failed = append(f.failed, id)
	return nil
}

type fakeFileLister struct {
	files []inbound.ReceivedAttachment
	err   error
	calls int
}

func (f *fakeFileLister) ListAttachments(context.Context, string) ([]inbound.ReceivedAttachment, error) {
	f.calls++
	return f.files, f.err
}

type fakeFileDownloader struct {
	data   []byte
	err    error
	calls  int
	gotURL string
	gotMax int64
}

func (f *fakeFileDownloader) Download(_ context.Context, url string, maxBytes int64) ([]byte, error) {
	f.calls++
	f.gotURL, f.gotMax = url, maxBytes
	return f.data, f.err
}

type fakeFileUploader struct {
	err                  error
	calls                int
	ref, id, contentType string
	data                 []byte
}

func (f *fakeFileUploader) UploadAttachment(_ context.Context, ref, attachmentID, contentType string, data []byte) error {
	f.calls++
	f.ref, f.id, f.contentType, f.data = ref, attachmentID, contentType, data
	return f.err
}

var errPermanent = errors.New("the backend said no")

func retryable(msg string) error { return &inbound.RetryableError{Err: errors.New(msg)} }

type inboundRig struct {
	jobs       *fakeJobStore
	lister     *fakeFileLister
	downloader *fakeFileDownloader
	uploader   *fakeFileUploader
}

func newInboundRig(job inbound.Job) *inboundRig {
	return &inboundRig{
		jobs: &fakeJobStore{claim: []inbound.Job{job}},
		lister: &fakeFileLister{files: []inbound.ReceivedAttachment{{
			ID: job.AttachmentID, Filename: "plan.pdf", Size: 5, ContentType: "application/pdf", DownloadURL: "https://inbound-cdn.resend.com/plan",
		}}},
		downloader: &fakeFileDownloader{data: []byte("%PDF-")},
		uploader:   &fakeFileUploader{},
	}
}

func (r *inboundRig) run() {
	InboundAttachmentWorker{Jobs: r.jobs, Lister: r.lister, Downloader: r.downloader, Uploader: r.uploader}.pollOnce(context.Background())
}

func testJob() inbound.Job {
	return inbound.Job{ID: "job-1", EmailID: "em_1", AttachmentID: "att_1", UploadRef: "r-abcdefghijklmnopqrst", Attempts: 2}
}

func TestInboundAttachmentWorker_CopiesTheFileAndCompletesTheJob(t *testing.T) {
	r := newInboundRig(testJob())

	r.run()

	if r.downloader.gotURL != "https://inbound-cdn.resend.com/plan" || r.downloader.gotMax != inbound.MaxFileBytes {
		t.Errorf("downloaded %q with limit %d, want the listed link and the file limit", r.downloader.gotURL, r.downloader.gotMax)
	}
	u := r.uploader
	if u.calls != 1 || u.ref != "r-abcdefghijklmnopqrst" || u.id != "att_1" || u.contentType != "application/pdf" || string(u.data) != "%PDF-" {
		t.Errorf("uploaded %+v, want the bytes under the job's ref and id with the listed content type", u)
	}
	if len(r.jobs.completed) != 1 || r.jobs.completed[0] != "job-1" || len(r.jobs.retried) != 0 || len(r.jobs.failed) != 0 {
		t.Errorf("settled: completed %v, retried %v, failed %v", r.jobs.completed, r.jobs.retried, r.jobs.failed)
	}
}

func TestInboundAttachmentWorker_AskedForAFreshLinkOnEveryAttempt(t *testing.T) {
	r := newInboundRig(testJob())
	r.run()
	if r.lister.calls != 1 {
		t.Fatalf("listed %d times, want once per attempt (links expire, so none is stored)", r.lister.calls)
	}
}

func TestInboundAttachmentWorker_FilesThatCanNeverBeCopiedAreDroppedNotRetried(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *inboundRig)
	}{
		{"the provider no longer lists it", func(r *inboundRig) { r.lister.files = nil }},
		{"it is listed as too large", func(r *inboundRig) { r.lister.files[0].Size = inbound.MaxFileBytes + 1 }},
		{"the download turns out too large", func(r *inboundRig) { r.downloader.err = inbound.ErrFileTooLarge }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newInboundRig(testJob())
			tc.setup(r)

			r.run()

			if len(r.jobs.completed) != 1 || len(r.jobs.retried) != 0 || len(r.jobs.failed) != 0 {
				t.Errorf("settled: completed %v, retried %v, failed %v; want just completed", r.jobs.completed, r.jobs.retried, r.jobs.failed)
			}
			if r.uploader.calls != 0 {
				t.Errorf("uploaded %d times, want never", r.uploader.calls)
			}
		})
	}
}

func TestInboundAttachmentWorker_AFileListedAsTooLargeIsNotDownloaded(t *testing.T) {
	r := newInboundRig(testJob())
	r.lister.files[0].Size = inbound.MaxFileBytes + 1

	r.run()

	if r.downloader.calls != 0 {
		t.Fatalf("downloaded %d times, want never for a file known to be too large", r.downloader.calls)
	}
}

func TestInboundAttachmentWorker_TemporaryFailuresAreRetriedWithTheAttemptCounted(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *inboundRig)
	}{
		{"listing", func(r *inboundRig) { r.lister.err = retryable("resend is down") }},
		{"downloading", func(r *inboundRig) { r.downloader.err = retryable("link expired") }},
		{"uploading", func(r *inboundRig) { r.uploader.err = retryable("backend is starting") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newInboundRig(testJob()) // 2 attempts so far
			tc.setup(r)

			r.run()

			if len(r.jobs.retried) != 1 || r.jobs.retried[0].id != "job-1" || r.jobs.retried[0].attempts != 3 || r.jobs.retried[0].message == "" {
				t.Fatalf("retried = %+v, want job-1 with attempt 3 and the reason", r.jobs.retried)
			}
			if len(r.jobs.completed) != 0 || len(r.jobs.failed) != 0 {
				t.Errorf("completed %v, failed %v; want neither", r.jobs.completed, r.jobs.failed)
			}
		})
	}
}

func TestInboundAttachmentWorker_PermanentFailuresGiveUp(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *inboundRig)
	}{
		{"listing", func(r *inboundRig) { r.lister.err = errPermanent }},
		{"downloading", func(r *inboundRig) { r.downloader.err = errPermanent }},
		{"uploading", func(r *inboundRig) { r.uploader.err = errPermanent }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newInboundRig(testJob())
			tc.setup(r)

			r.run()

			if len(r.jobs.failed) != 1 || r.jobs.failed[0] != "job-1" {
				t.Fatalf("failed = %v, want job-1", r.jobs.failed)
			}
			if len(r.jobs.completed) != 0 || len(r.jobs.retried) != 0 {
				t.Errorf("completed %v, retried %v; want neither", r.jobs.completed, r.jobs.retried)
			}
		})
	}
}

func TestInboundAttachmentWorker_AClaimFailureDoesNothing(t *testing.T) {
	r := newInboundRig(testJob())
	r.jobs.claimErr = errors.New("database is down")

	r.run()

	if r.lister.calls != 0 || r.uploader.calls != 0 || len(r.jobs.completed)+len(r.jobs.retried)+len(r.jobs.failed) != 0 {
		t.Fatal("a failed claim must not touch anything")
	}
}

func TestInboundAttachmentWorker_HandlesEveryClaimedJob(t *testing.T) {
	r := newInboundRig(testJob())
	second := testJob()
	second.ID, second.AttachmentID = "job-2", "att_2"
	r.jobs.claim = append(r.jobs.claim, second)
	r.lister.files = append(r.lister.files, inbound.ReceivedAttachment{
		ID: "att_2", Filename: "kitchen.jpg", Size: 3, ContentType: "image/jpeg", DownloadURL: "https://inbound-cdn.resend.com/k",
	})

	r.run()

	if len(r.jobs.completed) != 2 || r.uploader.calls != 2 {
		t.Fatalf("completed %v, uploaded %d times; want both jobs handled", r.jobs.completed, r.uploader.calls)
	}
}
