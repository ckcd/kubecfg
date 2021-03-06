package kubecfg

import (
	"encoding/json"
	"fmt"
	"sort"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/ksonnet/kubecfg/utils"
)

const (
	// AnnotationGcTag annotation that triggers
	// garbage collection. Objects with value equal to
	// command-line flag that are *not* in config will be deleted.
	//
	// NB: this is in phase1 of a migration to use a label instead.
	// At this stage, both label+migration are written, but the
	// annotation (only) is still used to trigger GC. [gctag-migration]
	AnnotationGcTag = LabelGcTag

	// LabelGcTag label that triggers garbage collection. Objects
	// with value equal to command-line flag that are *not* in
	// config will be deleted.
	//
	// NB: this is in phase1 of a migration from an annotation.
	// At this stage, both label+migration are written, but the
	// annotation (only) is still used to trigger GC. [gctag-migration]
	LabelGcTag = "kubecfg.ksonnet.io/garbage-collect-tag"

	// AnnotationGcStrategy controls gc logic.  Current values:
	// `auto` (default if absent) - do garbage collection
	// `ignore` - never garbage collect this object
	AnnotationGcStrategy = "kubecfg.ksonnet.io/garbage-collect-strategy"

	// GcStrategyAuto is the default automatic gc logic
	GcStrategyAuto = "auto"
	// GcStrategyIgnore means this object should be ignored by garbage collection
	GcStrategyIgnore = "ignore"
)

// UpdateCmd represents the update subcommand
type UpdateCmd struct {
	Client           dynamic.Interface
	Mapper           meta.RESTMapper
	Discovery        discovery.DiscoveryInterface
	DefaultNamespace string

	Create bool
	GcTag  string
	SkipGc bool
	DryRun bool
}

func (c UpdateCmd) Run(apiObjects []*unstructured.Unstructured) error {
	dryRunText := ""
	if c.DryRun {
		dryRunText = " (dry-run)"
	}

	log.Infof("Fetching schemas for %d resources", len(apiObjects))
	depOrder, err := utils.DependencyOrder(c.Discovery, c.Mapper, apiObjects)
	if err != nil {
		return err
	}
	sort.Sort(depOrder)

	seenUids := sets.NewString()

	for _, obj := range apiObjects {
		if c.GcTag != "" {
			// [gctag-migration]: Remove annotation in phase2
			utils.SetMetaDataAnnotation(obj, AnnotationGcTag, c.GcTag)
			utils.SetMetaDataLabel(obj, LabelGcTag, c.GcTag)
		}

		desc := fmt.Sprintf("%s %s", utils.ResourceNameFor(c.Mapper, obj), utils.FqName(obj))
		log.Info("Updating ", desc, dryRunText)

		rc, err := utils.ClientForResource(c.Client, c.Mapper, obj, c.DefaultNamespace)
		if err != nil {
			return err
		}

		asPatch, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		var newobj metav1.Object
		if !c.DryRun {
			newobj, err = rc.Patch(obj.GetName(), types.MergePatchType, asPatch, metav1.UpdateOptions{})
			log.Debugf("Patch(%s) returned (%v, %v)", obj.GetName(), newobj, err)
		} else {
			newobj, err = rc.Get(obj.GetName(), metav1.GetOptions{})
		}
		if c.Create && errors.IsNotFound(err) {
			log.Info(" Creating non-existent ", desc, dryRunText)
			if !c.DryRun {
				newobj, err = rc.Create(obj, metav1.CreateOptions{})
				log.Debugf("Create(%s) returned (%v, %v)", obj.GetName(), newobj, err)
			} else {
				newobj = obj
				err = nil
			}
		}
		if err != nil {
			// TODO: retry
			return fmt.Errorf("Error updating %s: %s", desc, err)
		}

		log.Debug("Updated object: ", diff.ObjectDiff(obj, newobj))

		// Some objects appear under multiple kinds
		// (eg: Deployment is both extensions/v1beta1
		// and apps/v1beta1).  UID is the only stable
		// identifier that links these two views of
		// the same object.
		seenUids.Insert(string(newobj.GetUID()))
	}

	if c.GcTag != "" && !c.SkipGc {
		version, err := utils.FetchVersion(c.Discovery)
		if err != nil {
			version = utils.GetDefaultVersion()
			log.Warnf("Unable to parse server version. Received %v. Using default %s", err, version.String())
		}

		// [gctag-migration]: Add LabelGcTag==c.GcTag to ListOptions.LabelSelector in phase2
		err = walkObjects(c.Client, c.Discovery, metav1.ListOptions{}, func(o runtime.Object) error {
			meta, err := meta.Accessor(o)
			if err != nil {
				return err
			}
			gvk := o.GetObjectKind().GroupVersionKind()
			desc := fmt.Sprintf("%s %s (%s)", utils.ResourceNameFor(c.Mapper, o), utils.FqName(meta), gvk.GroupVersion())
			log.Debugf("Considering %v for gc", desc)
			if eligibleForGc(meta, c.GcTag) && !seenUids.Has(string(meta.GetUID())) {
				log.Info("Garbage collecting ", desc, dryRunText)
				if !c.DryRun {
					err := gcDelete(c.Client, c.Mapper, &version, o)
					if err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func stringListContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func gcDelete(client dynamic.Interface, mapper meta.RESTMapper, version *utils.ServerVersion, o runtime.Object) error {
	obj, err := meta.Accessor(o)
	if err != nil {
		return fmt.Errorf("Unexpected object type: %s", err)
	}

	uid := obj.GetUID()
	desc := fmt.Sprintf("%s %s", utils.ResourceNameFor(mapper, o), utils.FqName(obj))

	deleteOpts := metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}
	if version.Compare(1, 6) < 0 {
		// 1.5.x option
		boolFalse := false
		deleteOpts.OrphanDependents = &boolFalse
	} else {
		// 1.6.x option (NB: Background is broken)
		fg := metav1.DeletePropagationForeground
		deleteOpts.PropagationPolicy = &fg
	}

	c, err := utils.ClientForResource(client, mapper, o, metav1.NamespaceNone)
	if err != nil {
		return err
	}

	err = c.Delete(obj.GetName(), &deleteOpts)
	if err != nil && (errors.IsNotFound(err) || errors.IsConflict(err)) {
		// We lost a race with something else changing the object
		log.Debugf("Ignoring error while deleting %s: %s", desc, err)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("Error deleting %s: %s", desc, err)
	}

	return nil
}

func walkObjects(client dynamic.Interface, disco discovery.DiscoveryInterface, listopts metav1.ListOptions, callback func(runtime.Object) error) error {
	rsrclists, err := disco.ServerResources()
	if err != nil {
		return err
	}
	for _, rsrclist := range rsrclists {
		gv, err := schema.ParseGroupVersion(rsrclist.GroupVersion)
		if err != nil {
			return err
		}

		for _, rsrc := range rsrclist.APIResources {
			if !stringListContains(rsrc.Verbs, "list") {
				log.Debugf("Don't know how to list %#v, skipping", rsrc)
				continue
			}

			gvr := gv.WithResource(rsrc.Name)
			if rsrc.Group != "" {
				gvr.Group = rsrc.Group
			}
			if rsrc.Version != "" {
				gvr.Version = rsrc.Version
			}

			var rc dynamic.ResourceInterface
			if rsrc.Namespaced {
				rc = client.Resource(gvr).Namespace(metav1.NamespaceAll)
			} else {
				rc = client.Resource(gvr)
			}

			log.Debugf("Listing %s", gvr)
			obj, err := rc.List(listopts)
			if err != nil {
				return err
			}
			if err = meta.EachListItem(obj, callback); err != nil {
				return err
			}
		}
	}
	return nil
}

func eligibleForGc(obj metav1.Object, gcTag string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			// Has a controller ref
			return false
		}
	}

	a := obj.GetAnnotations()

	strategy, ok := a[AnnotationGcStrategy]
	if !ok {
		strategy = GcStrategyAuto
	}

	// [gctag-migration]: Check *label* == tag instead in phase2
	return a[AnnotationGcTag] == gcTag &&
		strategy == GcStrategyAuto
}
