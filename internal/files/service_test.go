package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var errServiceFault = errors.New("injected backend failure")

// The fake persists state across Service instances and supports failures on both
// sides of commit, so these tests exercise uncertain outcomes and compensation.
type lifecycleRepository struct {
	mu                                                                sync.Mutex
	files                                                             map[string]File
	createError, activateError, abandonError, beginError, finishError bool
	commitCreate, commitActivate, commitBegin, commitFinish           bool
	claimUploading                                                    bool
	readyError                                                        error
	cleanupContextChecked                                             bool
	leaseClaims                                                       bool
	claimed                                                           map[string]bool
}

func newLifecycleRepository() *lifecycleRepository {
	return &lifecycleRepository{files: make(map[string]File)}
}

func (r *lifecycleRepository) Create(ctx context.Context, p CreateFileParams) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f := File{ID: p.ID, StorageKey: p.StorageKey, OriginalName: p.OriginalName, ContentType: p.ContentType, Size: p.Size, State: "uploading", CreatedAt: time.Now()}
	if !r.createError || r.commitCreate {
		r.files[f.ID] = f
	}
	if r.createError {
		return File{}, errServiceFault
	}
	return f, nil
}
func (r *lifecycleRepository) Activate(ctx context.Context, id string) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f := r.files[id]
	if f.State != "uploading" {
		return File{}, ErrOperationConflict
	}
	if !r.activateError || r.commitActivate {
		f.State = "ready"
		r.files[id] = f
	}
	if r.activateError {
		return File{}, errServiceFault
	}
	return f, nil
}
func (r *lifecycleRepository) AbandonUpload(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > CleanupTimeout {
		return false, errors.New("cleanup context must have its own bounded deadline")
	}
	r.cleanupContextChecked = true
	if r.abandonError {
		return false, errServiceFault
	}
	f := r.files[id]
	if f.State != "uploading" {
		return false, nil
	}
	f.State = "deleting"
	r.files[id] = f
	return true, nil
}
func (r *lifecycleRepository) List(ctx context.Context) ([]File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []File{}
	for _, f := range r.files {
		if f.State == "ready" {
			out = append(out, f)
		}
	}
	return out, nil
}
func (r *lifecycleRepository) GetByID(ctx context.Context, id string) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	if !ok || f.State != "ready" {
		return File{}, ErrFileNotFound
	}
	return f, nil
}
func (r *lifecycleRepository) BeginDelete(ctx context.Context, id string) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f, ok := r.files[id]
	if !ok {
		return File{}, ErrFileNotFound
	}
	if !r.beginError || r.commitBegin {
		if f.State != "deleted" {
			f.State = "deleting"
		}
		r.files[id] = f
	}
	if r.beginError {
		return File{}, errServiceFault
	}
	return f, nil
}
func (r *lifecycleRepository) MarkDeleted(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	f := r.files[id]
	if f.State != "deleting" && f.State != "deleted" {
		return ErrOperationConflict
	}
	if !r.finishError || r.commitFinish {
		f.State = "deleted"
		r.files[id] = f
	}
	if r.finishError {
		return errServiceFault
	}
	return nil
}
func (r *lifecycleRepository) ClaimCleanup(ctx context.Context, stale time.Duration, limit int) ([]File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []File{}
	for id, f := range r.files {
		if r.leaseClaims && r.claimed[id] {
			continue
		}
		if f.State == "deleting" || f.State == "deleted" || (f.State == "uploading" && r.claimUploading) {
			if r.leaseClaims {
				if r.claimed == nil {
					r.claimed = make(map[string]bool)
				}
				r.claimed[id] = true
			}
			f.State = "deleting"
			r.files[id] = f
			out = append(out, f)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (r *lifecycleRepository) Ready(context.Context) error { return r.readyError }

type lifecycleStorage struct {
	mu                                                   sync.Mutex
	objects                                              map[string][]byte
	uploadError, deleteError, commitUpload, commitDelete bool
	uploadHook                                           func(context.Context)
	readyError                                           error
	deletes                                              int
}

func newLifecycleStorage() *lifecycleStorage {
	return &lifecycleStorage{objects: make(map[string][]byte)}
}
func (s *lifecycleStorage) Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if s.uploadHook != nil {
		s.uploadHook(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.uploadError || s.commitUpload {
		s.objects[key] = data
	}
	if s.uploadError {
		return errServiceFault
	}
	return nil
}
func (s *lifecycleStorage) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, errServiceFault
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
func (s *lifecycleStorage) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	if !s.deleteError || s.commitDelete {
		delete(s.objects, key)
	}
	if s.deleteError {
		return errServiceFault
	}
	return nil
}
func (s *lifecycleStorage) Ready(context.Context) error { return s.readyError }

type lifecycleMultipart struct{ *bytes.Reader }

func (lifecycleMultipart) Close() error { return nil }
func uploadLifecycle(t *testing.T, service *Service, ctx context.Context, data []byte) (File, error) {
	t.Helper()
	return service.Upload(ctx, lifecycleMultipart{bytes.NewReader(data)}, &multipart.FileHeader{Filename: "данные\".txt", Size: int64(len(data)), Header: textproto.MIMEHeader{"Content-Type": []string{"text/plain"}}})
}

func TestServiceUploadRoundTripAndBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 8, 9} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			r := newLifecycleRepository()
			s := newLifecycleStorage()
			service := NewService(r, s, 8)
			data := bytes.Repeat([]byte{0x93}, n)
			file, err := uploadLifecycle(t, service, context.Background(), data)
			if n == 0 {
				if !errors.Is(err, ErrEmptyFile) {
					t.Fatalf("empty error=%v", err)
				}
				return
			}
			if n > 8 {
				if !errors.Is(err, ErrFileTooLarge) {
					t.Fatalf("oversize error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if file.State != "ready" {
				t.Fatalf("state=%s", file.State)
			}
			if file.StorageKey != file.ID {
				t.Fatalf("key is not generated independently of filename: %q", file.StorageKey)
			}
			_, body, err := service.Download(context.Background(), file.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("roundtrip=%v, %v", got, err)
			}
		})
	}
}

func TestServiceUploadFaultsAndUncertainCommits(t *testing.T) {
	cases := []struct {
		name                  string
		setup                 func(*lifecycleRepository, *lifecycleStorage)
		wantReady, wantObject bool
	}{
		{"create fails", func(r *lifecycleRepository, s *lifecycleStorage) { r.createError = true }, false, false},
		{"create commits but acknowledgement lost", func(r *lifecycleRepository, s *lifecycleStorage) { r.createError = true; r.commitCreate = true }, false, false},
		{"storage fails", func(r *lifecycleRepository, s *lifecycleStorage) { s.uploadError = true }, false, false},
		{"storage writes but acknowledgement lost", func(r *lifecycleRepository, s *lifecycleStorage) { s.uploadError = true; s.commitUpload = true }, false, false},
		{"activation fails after storage success", func(r *lifecycleRepository, s *lifecycleStorage) { r.activateError = true }, false, false},
		{"activation commits but acknowledgement lost", func(r *lifecycleRepository, s *lifecycleStorage) { r.activateError = true; r.commitActivate = true }, true, true},
		{"compensation database unavailable", func(r *lifecycleRepository, s *lifecycleStorage) { r.activateError = true; r.abandonError = true }, false, true},
		{"compensation storage unavailable", func(r *lifecycleRepository, s *lifecycleStorage) { r.activateError = true; s.deleteError = true }, false, true},
		{"compensation completion fails", func(r *lifecycleRepository, s *lifecycleStorage) { r.activateError = true; r.finishError = true }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepository()
			s := newLifecycleStorage()
			tc.setup(r, s)
			service := NewService(r, s, 8)
			if _, err := uploadLifecycle(t, service, context.Background(), []byte("payload")); err == nil {
				t.Fatal("expected injected error")
			}
			listed, _ := service.List(context.Background())
			if (len(listed) > 0) != tc.wantReady {
				t.Fatalf("visible files=%v", listed)
			}
			if (len(s.objects) > 0) != tc.wantObject {
				t.Fatalf("objects=%v", s.objects)
			}
			// A fresh service recovers hidden state while keeping uncertain-but-ready
			// uploads accessible. Unit fake makes stale/retry deadlines immediately due.
			r.createError = false
			r.activateError = false
			r.abandonError = false
			r.finishError = false
			r.claimUploading = true
			s.deleteError = false
			if err := NewService(r, s, 8).Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if (len(s.objects) > 0) != tc.wantReady {
				t.Fatalf("objects after recovery=%v", s.objects)
			}
		})
	}
}

func TestServiceUploadCancellationHasIndependentCompensation(t *testing.T) {
	r := newLifecycleRepository()
	s := newLifecycleStorage()
	ctx, cancel := context.WithCancel(context.Background())
	s.uploadHook = func(context.Context) { cancel() }
	_, err := uploadLifecycle(t, NewService(r, s, 8), ctx, []byte("payload"))
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("error=%v", err)
	}
	if !r.cleanupContextChecked {
		t.Fatal("compensation did not receive an independent bounded context")
	}
	for _, f := range r.files {
		if f.State != "deleted" {
			t.Fatalf("state=%s", f.State)
		}
	}
}

func TestServiceDeleteFaultsAndRecovery(t *testing.T) {
	cases := []struct {
		name                  string
		setup                 func(*lifecycleRepository, *lifecycleStorage)
		wantReady, wantObject bool
	}{
		{"database before transition", func(r *lifecycleRepository, s *lifecycleStorage) { r.beginError = true }, true, true},
		{"database after transition commit", func(r *lifecycleRepository, s *lifecycleStorage) { r.beginError = true; r.commitBegin = true }, false, true},
		{"storage before deletion", func(r *lifecycleRepository, s *lifecycleStorage) { s.deleteError = true }, false, true},
		{"storage after deletion", func(r *lifecycleRepository, s *lifecycleStorage) { s.deleteError = true; s.commitDelete = true }, false, false},
		{"database before completion", func(r *lifecycleRepository, s *lifecycleStorage) { r.finishError = true }, false, false},
		{"database after completion commit", func(r *lifecycleRepository, s *lifecycleStorage) { r.finishError = true; r.commitFinish = true }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepository()
			s := newLifecycleStorage()
			service := NewService(r, s, 8)
			f, err := uploadLifecycle(t, service, context.Background(), []byte("payload"))
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(r, s)
			if err := service.Delete(context.Background(), f.ID); err == nil {
				t.Fatal("expected injected failure")
			}
			listed, _ := service.List(context.Background())
			if (len(listed) > 0) != tc.wantReady {
				t.Fatalf("visible=%v", listed)
			}
			if (len(s.objects) > 0) != tc.wantObject {
				t.Fatalf("objects=%v", s.objects)
			}
			r.beginError = false
			r.finishError = false
			s.deleteError = false
			if err := service.Delete(context.Background(), f.ID); err != nil {
				t.Fatal(err)
			}
			if err := NewService(r, s, 8).Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.objects) != 0 {
				t.Fatalf("objects survived=%v", s.objects)
			}
		})
	}
}

func TestServiceConcurrentRepeatedAndAbsentDelete(t *testing.T) {
	r := newLifecycleRepository()
	s := newLifecycleStorage()
	service := NewService(r, s, 8)
	f, err := uploadLifecycle(t, service, context.Background(), []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- service.Delete(context.Background(), f.ID) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{f.ID, uuid.NewString()} {
		if err := service.Delete(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := service.Download(context.Background(), f.ID); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("download error=%v", err)
	}
	if err := service.Delete(context.Background(), "../../secrets"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid ID error=%v", err)
	}
}

func TestServiceRecoveryDeletesLatePutAndPreservesReady(t *testing.T) {
	r := newLifecycleRepository()
	s := newLifecycleStorage()
	service := NewService(r, s, 8)
	ready, err := uploadLifecycle(t, service, context.Background(), []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	staleID := uuid.NewString()
	r.files[staleID] = File{ID: staleID, StorageKey: staleID, State: "uploading"}
	r.claimUploading = true
	if err := NewService(r, s, 8).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The remote PUT can complete even after a timeout and an initial DELETE.
	s.objects[staleID] = []byte("late write")
	if err := NewService(r, s, 8).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.objects) != 1 || s.objects[ready.ID] == nil {
		t.Fatalf("objects after restart/recovery=%v", s.objects)
	}
	if _, _, err := service.Download(context.Background(), staleID); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("stale file exposed: %v", err)
	}
}

func TestServiceReadinessChecksBothDependencies(t *testing.T) {
	r := newLifecycleRepository()
	s := newLifecycleStorage()
	service := NewService(r, s, 8)
	if err := service.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.readyError = errServiceFault
	if err := service.Ready(context.Background()); err == nil {
		t.Fatal("database failure ignored")
	}
	r.readyError = nil
	s.readyError = errServiceFault
	if err := service.Ready(context.Background()); err == nil {
		t.Fatal("storage failure ignored")
	}
}

func TestServiceCleanupClaimWinsAgainstLateUploadActivation(t *testing.T) {
	r := newLifecycleRepository()
	s := newLifecycleStorage()
	service := NewService(r, s, 8)
	started, release := make(chan struct{}), make(chan struct{})
	s.uploadHook = func(context.Context) {
		close(started)
		<-release
	}
	result := make(chan error, 1)
	go func() {
		_, err := uploadLifecycle(t, service, context.Background(), []byte("payload"))
		result <- err
	}()
	<-started // Durable intent exists; the remote PUT has not completed yet.
	r.mu.Lock()
	r.claimUploading = true // Model an expired upload or a remote PUT outliving its deadline.
	r.mu.Unlock()
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("activation after cleanup must fail, got %v", err)
	}
	listed, err := service.List(context.Background())
	if err != nil || len(listed) != 0 {
		t.Fatalf("late upload became visible: %v, %v", listed, err)
	}
	if len(s.objects) != 1 {
		t.Fatal("expected the deliberately delayed remote PUT")
	}
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.objects) != 0 {
		t.Fatalf("tombstone sweep left late objects: %v", s.objects)
	}
}

func TestServiceTwoCleanupWorkersRespectClaimsAndBatchLimit(t *testing.T) {
	r := newLifecycleRepository()
	r.leaseClaims = true
	s := newLifecycleStorage()
	for i := 0; i < 64; i++ {
		id := uuid.NewString()
		r.files[id] = File{ID: id, StorageKey: id, State: "deleting"}
		s.objects[id] = []byte("payload")
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- NewService(r, s, 8).Reconcile(context.Background()) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(s.objects) != 0 || s.deletes != 64 || len(r.claimed) != 64 {
		t.Fatalf("objects=%d delete calls=%d claimed=%d", len(s.objects), s.deletes, len(r.claimed))
	}
}
