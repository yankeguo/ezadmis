package ezadmis

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	admissionv1 "k8s.io/api/admission/v1"
)

// WebhookResponseWriter response writer for WebhookHandler
type WebhookResponseWriter interface {
	// Deny deny this admission request
	Deny(deny string)
	// PatchRaw append a raw JSONPatch operation
	PatchRaw(patch map[string]interface{})
	// PatchAdd append a JSONPatch 'add' operation
	PatchAdd(path string, value interface{})
	// PatchRemove append a JSONPatch 'remove' operation
	PatchRemove(path string)
	// PatchReplace append a JSONPatch 'replace' operation
	PatchReplace(path string, value interface{})
	// PatchCopy append a JSONPatch 'copy' operation
	PatchCopy(path string, from string)
	// PatchMove append a JSONPatch 'move' operation
	PatchMove(path string, from string)
	// PatchTest append a JSONPatch 'test' operation
	PatchTest(path string, value interface{})
}

type webhookResponseWriter struct {
	patches []map[string]interface{}
	deny    string
}

func (w *webhookResponseWriter) Deny(deny string) {
	w.deny = deny
}

func (w *webhookResponseWriter) PatchRaw(patch map[string]interface{}) {
	w.patches = append(w.patches, patch)
}

func (w *webhookResponseWriter) PatchAdd(path string, value interface{}) {
	w.PatchRaw(map[string]interface{}{
		"op":    "add",
		"path":  path,
		"value": value,
	})
}

func (w *webhookResponseWriter) PatchRemove(path string) {
	w.PatchRaw(map[string]interface{}{
		"op":   "remove",
		"path": path,
	})
}

func (w *webhookResponseWriter) PatchReplace(path string, value interface{}) {
	w.PatchRaw(map[string]interface{}{
		"op":    "replace",
		"path":  path,
		"value": value,
	})
}

func (w *webhookResponseWriter) PatchCopy(path string, from string) {
	w.PatchRaw(map[string]interface{}{
		"op":   "copy",
		"path": path,
		"from": from,
	})
}

func (w *webhookResponseWriter) PatchMove(path string, from string) {
	w.PatchRaw(map[string]interface{}{
		"op":   "move",
		"path": path,
		"from": from,
	})
}

func (w *webhookResponseWriter) PatchTest(path string, value interface{}) {
	w.PatchRaw(map[string]interface{}{
		"op":    "test",
		"path":  path,
		"value": value,
	})
}

// WebhookHandler function to modify incoming kubernetes resource;
type WebhookHandler func(ctx context.Context, req *admissionv1.AdmissionRequest, rw WebhookResponseWriter) (err error)

// WrapWebhookHandlerOptions options for wrapping WebhookHandler
type WrapWebhookHandlerOptions struct {
	Debug bool
}

// WrapWebhookHandler wrap WebhookHandler to http.HandlerFunc
func WrapWebhookHandler(opts WrapWebhookHandlerOptions, handler WebhookHandler) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		// automatically error returning
		var err error
		defer func() {
			if err == nil {
				return
			}
			log.Println("failed to handle admission review:", err.Error())
			http.Error(rw, err.Error(), http.StatusInternalServerError)
		}()

		// decode request
		var reqReview admissionv1.AdmissionReview
		if err = json.NewDecoder(req.Body).Decode(&reqReview); err != nil {
			err = errors.New("failed to decode incoming AdmissionReview: " + err.Error())
			return
		}

		if opts.Debug {
			log.Println("Request:")
			raw, _ := json.Marshal(&reqReview)
			log.Println(string(raw))
		}

		// execute handler
		ret := &webhookResponseWriter{}

		if err = handler(req.Context(), reqReview.Request, ret); err != nil {
			err = errors.New("failed to execute WebhookHandler: " + err.Error())
			return
		}

		if opts.Debug {
			log.Println("Patches:")
			if len(ret.patches) == 0 {
				log.Println("--- NONE ---")
			} else {
				raw, _ := json.MarshalIndent(ret.patches, "", "  ")
				log.Println(string(raw))
			}
			log.Println("Deny:", ret.deny)
		}

		// build response
		var resReview admissionv1.AdmissionReview

		{
			var patch []byte
			var patchType *admissionv1.PatchType
			if len(ret.patches) != 0 {
				if patch, err = json.Marshal(ret.patches); err != nil {
					err = errors.New("failed to marshal WebhookHandler patches: " + err.Error())
					return
				}
				patchType = new(admissionv1.PatchType)
				*patchType = admissionv1.PatchTypeJSONPatch
			}

			var status *metav1.Status
			if ret.deny != "" {
				status = &metav1.Status{
					Status:  metav1.StatusFailure,
					Message: ret.deny,
					Reason:  metav1.StatusReasonBadRequest,
				}
			}

			resReview = admissionv1.AdmissionReview{
				TypeMeta: reqReview.TypeMeta,
				Response: &admissionv1.AdmissionResponse{
					UID:       reqReview.Request.UID,
					Allowed:   ret.deny == "",
					Result:    status,
					Patch:     patch,
					PatchType: patchType,
				},
			}
		}

		// send response
		var buf []byte
		if buf, err = json.Marshal(resReview); err != nil {
			err = errors.New("failed to marshal outgoing AdmissionReview: " + err.Error())
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Content-Length", strconv.Itoa(len(buf)))
		_, _ = rw.Write(buf)
	}
}

// WebhookServer webhook server abstraction
type WebhookServer interface {
	// ListenAndServe wraps internal http.Server#ListenAndServeTLS()
	ListenAndServe() error

	// ListenAndServeGracefully ListenAndServe() with signal handling, perfect for using
	// inside main() as the only component
	ListenAndServeGracefully() error

	// Shutdown wraps internal http.Server#Shutdown()
	Shutdown(ctx context.Context) error
}

// WebhookServerOptions options for WebhookServer
type WebhookServerOptions struct {
	Port     int
	CertFile string
	KeyFile  string
	Debug    bool
	Handler  WebhookHandler
}

var (
	defaultWebhookServerOptions = WebhookServerOptions{
		Port:     443,
		CertFile: "/admission-server/tls.crt",
		KeyFile:  "/admission-server/tls.key",
	}
)

// DefaultWebhookServerOptions returns default options for WebhookServer
func DefaultWebhookServerOptions() WebhookServerOptions {
	return defaultWebhookServerOptions
}

type webhookServer struct {
	opts WebhookServerOptions
	s    *http.Server
}

func (w *webhookServer) ListenAndServe() error {
	return w.s.ListenAndServeTLS(w.opts.CertFile, w.opts.KeyFile)
}

func (w *webhookServer) ListenAndServeGracefully() (err error) {
	chErr := make(chan error, 1)
	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		chErr <- w.ListenAndServe()
	}()

	select {
	case err = <-chErr:
	case sig := <-chSig:
		log.Println("signal caught:", sig.String())
		err = w.Shutdown(context.Background())
	}
	return
}

func (w *webhookServer) Shutdown(ctx context.Context) error {
	return w.s.Shutdown(ctx)
}

// NewWebhookServer create a WebhookServer
func NewWebhookServer(opts WebhookServerOptions) WebhookServer {
	dfo := DefaultWebhookServerOptions()
	if opts.Port == 0 {
		opts.Port = dfo.Port
	}
	if opts.CertFile == "" {
		opts.CertFile = dfo.CertFile
	}
	if opts.KeyFile == "" {
		opts.KeyFile = dfo.KeyFile
	}
	if opts.Handler == nil {
		opts.Handler = func(_ context.Context, _ *admissionv1.AdmissionRequest, _ WebhookResponseWriter) error {
			return nil
		}
	}
	return &webhookServer{
		opts: opts,
		s: &http.Server{
			Addr: ":" + strconv.Itoa(opts.Port),
			Handler: WrapWebhookHandler(
				WrapWebhookHandlerOptions{
					Debug: opts.Debug,
				},
				opts.Handler,
			),
		},
	}
}
