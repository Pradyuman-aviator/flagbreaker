package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

// Annotations a team puts on a workload instead of writing FlagGuards.
const (
	// AnnotationFlags lists the Flagsmith flags the workload depends on,
	// comma-separated. It is the only one required.
	AnnotationFlags = "flagbreaker.dev/flags"
	// AnnotationEnvironmentKey is the client-side key of the Flagsmith
	// environment. It defaults to the controller's --default-environment-key.
	AnnotationEnvironmentKey = "flagbreaker.dev/environment-key"
	// AnnotationMode is watch or act, like spec.mode.
	AnnotationMode = "flagbreaker.dev/mode"
	// AnnotationStableFor is spec.stableFor, such as 30s.
	AnnotationStableFor = "flagbreaker.dev/stable-for"
	// AnnotationRunbook is a URL added to every Feature Health event.
	AnnotationRunbook = "flagbreaker.dev/runbook-url"

	// LabelGenerated marks FlagGuards made from annotations.
	LabelGenerated = "flagbreaker.dev/generated"
)

// AnnotationReconciler keeps one FlagGuard per flag listed in a workload's
// flagbreaker.dev/flags annotation. The guards are owned by the workload, so
// deleting the workload deletes them; removing a flag from the annotation
// deletes its guard.
type AnnotationReconciler struct {
	client.Client
	Recorder recorder.EventRecorder
	// Kind is Deployment, StatefulSet or DaemonSet.
	Kind string
	// DefaultEnvironmentKey is used when a workload doesn't set
	// flagbreaker.dev/environment-key.
	DefaultEnvironmentKey string
}

func (r *AnnotationReconciler) newObject() client.Object {
	switch r.Kind {
	case KindStatefulSet:
		return &appsv1.StatefulSet{}
	case KindDaemonSet:
		return &appsv1.DaemonSet{}
	default:
		return &appsv1.Deployment{}
	}
}

// Reconcile creates, updates and deletes one workload's generated guards.
func (r *AnnotationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		// A deleted workload's guards are garbage-collected with it.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	desired, problem := r.desiredGuards(obj)
	if problem != "" {
		r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, "InvalidFlagbreakerAnnotation", "Annotate", "%s", problem)
	}

	var existing v1alpha1.FlagGuardList
	if err := r.List(ctx, &existing, client.InNamespace(obj.GetNamespace()), client.MatchingLabels{LabelGenerated: "true"}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range existing.Items {
		g := &existing.Items[i]
		if !ownedBy(g, obj) {
			continue
		}
		want, ok := desired[g.Name]
		if !ok {
			if err := r.Delete(ctx, g); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		delete(desired, g.Name)
		if managedSpecEqual(g.Spec, want.Spec) {
			continue
		}
		copyManagedSpec(&g.Spec, want.Spec)
		if err := r.Update(ctx, g); err != nil {
			return ctrl.Result{}, err
		}
	}

	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g := desired[name]
		if err := r.Create(ctx, g); apierrors.IsAlreadyExists(err) {
			// A FlagGuard someone wrote by hand has this name; leave it be.
			r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, "FlagGuardExists", "Annotate",
				"a FlagGuard named %s already exists and wasn't made from this annotation, so it is left alone", name)
		} else if err != nil {
			return ctrl.Result{}, err
		} else {
			logf.FromContext(ctx).Info("created FlagGuard from annotation", "guard", name, "flag", g.Spec.Flag)
		}
	}
	return ctrl.Result{}, nil
}

// desiredGuards builds the guards the annotations ask for, keyed by name.
func (r *AnnotationReconciler) desiredGuards(obj client.Object) (map[string]*v1alpha1.FlagGuard, string) {
	out := map[string]*v1alpha1.FlagGuard{}
	ann := obj.GetAnnotations()
	flags := splitFlags(ann[AnnotationFlags])
	if len(flags) == 0 {
		return out, ""
	}
	key := strings.TrimSpace(ann[AnnotationEnvironmentKey])
	if key == "" {
		key = r.DefaultEnvironmentKey
	}
	if key == "" {
		return out, fmt.Sprintf("%s is set but there is no Flagsmith environment key: set %s, or the controller's --default-environment-key",
			AnnotationFlags, AnnotationEnvironmentKey)
	}

	var problems []string
	spec := v1alpha1.FlagGuardSpec{EnvironmentKey: key, Workload: v1alpha1.WorkloadReference{Kind: r.Kind, Name: obj.GetName()}}
	switch m := v1alpha1.Mode(strings.TrimSpace(ann[AnnotationMode])); m {
	case "", v1alpha1.ModeWatch, v1alpha1.ModeAct:
		spec.Mode = m
	default:
		spec.Mode = v1alpha1.ModeWatch
		problems = append(problems, fmt.Sprintf("%s is %q; use watch or act. Using watch", AnnotationMode, m))
	}
	if s := strings.TrimSpace(ann[AnnotationStableFor]); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			spec.StableFor = &metav1.Duration{Duration: d}
		} else {
			problems = append(problems, fmt.Sprintf("%s is %q, not a duration such as 30s; using the default", AnnotationStableFor, s))
		}
	}
	if u := strings.TrimSpace(ann[AnnotationRunbook]); u != "" {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			spec.Links = []v1alpha1.Link{{Title: "Runbook", URL: u}}
		} else {
			problems = append(problems, fmt.Sprintf("%s must be an http(s) URL", AnnotationRunbook))
		}
	}

	for _, flag := range flags {
		name := guardName(obj.GetName(), flag)
		if _, taken := out[name]; taken {
			name = guardName(obj.GetName(), flag) + "-" + shortHash(flag)
		}
		g := &v1alpha1.FlagGuard{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: obj.GetNamespace(),
				Labels:    map[string]string{LabelGenerated: "true"},
			},
			Spec: spec,
		}
		g.Spec.Flag = flag
		g.Spec.Links = append([]v1alpha1.Link(nil), spec.Links...)
		// A plain owner reference, without blockOwnerDeletion, needs no extra
		// permission on the workload.
		if err := controllerutil.SetOwnerReference(obj, g, r.Scheme()); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out[name] = g
	}
	return out, strings.Join(problems, "; ")
}

func ownedBy(g *v1alpha1.FlagGuard, obj client.Object) bool {
	for _, ref := range g.OwnerReferences {
		if ref.UID == obj.GetUID() {
			return true
		}
	}
	return false
}

// managedSpecEqual compares the fields annotations control; the API server
// fills in the rest with defaults.
func managedSpecEqual(have, want v1alpha1.FlagGuardSpec) bool {
	merged := *have.DeepCopy()
	copyManagedSpec(&merged, want)
	return equality.Semantic.DeepEqual(merged, have)
}

func copyManagedSpec(dst *v1alpha1.FlagGuardSpec, src v1alpha1.FlagGuardSpec) {
	dst.Flag, dst.EnvironmentKey, dst.Workload, dst.Mode, dst.Links = src.Flag, src.EnvironmentKey, src.Workload, src.Mode, src.Links
	if src.StableFor != nil {
		dst.StableFor = src.StableFor
	}
}

func splitFlags(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

var notNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// guardName makes a valid object name from the workload and flag names, such
// as checkout-new-pricing.
func guardName(workload, flag string) string {
	f := strings.Trim(notNameChars.ReplaceAllString(strings.ToLower(flag), "-"), "-")
	if f == "" {
		f = shortHash(flag)
	}
	name := workload + "-" + f
	if len(name) > 200 {
		name = name[:190] + "-" + shortHash(name)
	}
	return name
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}

// SetupWithManager watches one workload kind.
func (r *AnnotationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("annotations-"+strings.ToLower(r.Kind)).
		For(r.newObject(), builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		// A generated guard someone edited or deleted is put back.
		Watches(&v1alpha1.FlagGuard{},
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), r.newObject()),
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, deletedPredicate()))).
		Complete(r)
}

func deletedPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
