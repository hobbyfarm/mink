package translation

import (
	"context"
	"strings"

	"github.com/hobbyfarm/mink/pkg/strategy"
	"github.com/hobbyfarm/mink/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

var _ strategy.CompleteStrategy = (*Strategy)(nil)

type Translator interface {
	FromPublicName(ctx context.Context, namespace, name string) (string, string, error)
	ListOpts(ctx context.Context, namespace string, opts storage.ListOptions) (string, storage.ListOptions, error)
	ToPublic(ctx context.Context, obj ...runtime.Object) ([]types.Object, error)
	FromPublic(ctx context.Context, obj runtime.Object) (types.Object, error)
	NewPublic() types.Object
	NewPublicList() types.ObjectList
}

type TranslationOptions struct {
	AppendPublicUIDSegment bool
}

func DefaultTranslationOptions() TranslationOptions {
	return TranslationOptions{
		AppendPublicUIDSegment: true,
	}
}

func NewTranslationStrategy(translator Translator, strategy strategy.CompleteStrategy, options TranslationOptions) *Strategy {
	return &Strategy{
		strategy:   strategy,
		translator: translator,
		options:    options,
		pubGVK:     types.MustGetGVK(translator.NewPublic(), strategy.Scheme()),
	}
}

type Strategy struct {
	strategy   strategy.CompleteStrategy
	translator Translator
	pubGVK     schema.GroupVersionKind
	options    TranslationOptions
}

func (t *Strategy) toPublicObjects(ctx context.Context, objs ...runtime.Object) ([]types.Object, error) {
	uids := map[ktypes.UID]bool{}
	for _, obj := range objs {
		uids[obj.(types.Object).GetUID()] = true
	}

	result, err := t.translator.ToPublic(ctx, objs...)
	if err != nil {
		return nil, err
	}
	for _, obj := range result {
		if t.options.AppendPublicUIDSegment {
			if uids[obj.GetUID()] {
				obj.SetUID(obj.GetUID() + "-p")
			}
		}

		// Reset the GVK to the public GVK
		obj.GetObjectKind().SetGroupVersionKind(t.pubGVK)
	}

	return result, nil
}

func (t *Strategy) toPublic(ctx context.Context, obj runtime.Object, err error, namespace, name string) (types.Object, error) {
	if err != nil {
		// if err is a not found error, translate its Kind back to the public version
		if apierrors.IsNotFound(err) {
			gvk, err := apiutil.GVKForObject(t.translator.NewPublic(), t.strategy.Scheme())
			if err != nil {
				return nil, err
			}

			return nil, apierrors.NewNotFound(schema.GroupResource{
				Group:    gvk.Group,
				Resource: strings.ToLower(guessPluralName(gvk.Kind)),
			}, name)
		}
		return nil, err
	}

	objs, err := t.toPublicObjects(ctx, obj)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if obj.GetNamespace() == namespace && obj.GetName() == name {
			return obj, nil
		}
	}
	if len(objs) > 0 {
		return objs[0], nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
}

func (t *Strategy) Create(ctx context.Context, object types.Object) (types.Object, error) {
	newObj, err := t.fromPublic(ctx, object)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.Create(ctx, newObj)
	return t.toPublic(ctx, o, err, object.GetNamespace(), object.GetName())
}

func (t *Strategy) New() types.Object {
	return t.translator.NewPublic()
}

func (t *Strategy) Get(ctx context.Context, namespace, name string) (types.Object, error) {
	newNamespace, newName, err := t.translator.FromPublicName(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.Get(ctx, newNamespace, newName)
	return t.toPublic(ctx, o, err, namespace, name)
}

func (t *Strategy) fromPublic(ctx context.Context, obj types.Object) (types.Object, error) {
	newObj, err := t.translator.FromPublic(ctx, obj)
	if err != nil {
		return nil, err
	}
	if t.options.AppendPublicUIDSegment {
		newObj.SetUID(ktypes.UID(strings.TrimSuffix(string(newObj.GetUID()), "-p")))
	}
	return newObj, nil
}

func (t *Strategy) Update(ctx context.Context, obj types.Object) (types.Object, error) {
	newObj, err := t.fromPublic(ctx, obj)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.Update(ctx, newObj)
	return t.toPublic(ctx, o, err, obj.GetNamespace(), obj.GetName())
}

func (t *Strategy) UpdateStatus(ctx context.Context, obj types.Object) (types.Object, error) {
	newObj, err := t.fromPublic(ctx, obj)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.UpdateStatus(ctx, newObj)
	if err != nil {
		return nil, err
	}
	objs, err := t.toPublicObjects(ctx, o)
	if err != nil {
		return nil, err
	}
	return objs[0], nil
}

func (t *Strategy) toPublicList(ctx context.Context, obj types.ObjectList) (types.ObjectList, error) {
	var (
		items      []runtime.Object
		list       = obj.(types.ObjectList)
		publicList = t.translator.NewPublicList()
	)

	err := meta.EachListItem(list, func(obj runtime.Object) error {
		items = append(items, obj)
		return nil
	})
	if err != nil {
		return nil, err
	}

	publicItems := make([]runtime.Object, 0, len(items))
	objs, err := t.toPublicObjects(ctx, items...)
	if err != nil {
		return nil, err
	}

	for _, obj := range objs {
		publicItems = append(publicItems, obj)
	}

	err = meta.SetList(publicList, publicItems)
	if err != nil {
		return nil, err
	}

	publicList.SetContinue(list.GetContinue())
	publicList.SetResourceVersion(list.GetResourceVersion())
	return publicList, nil
}

func (t *Strategy) List(ctx context.Context, namespace string, opts storage.ListOptions) (types.ObjectList, error) {
	namespace, opts, err := t.translateListOpts(ctx, namespace, opts)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.List(ctx, namespace, opts)
	if err != nil {
		return nil, err
	}
	return t.toPublicList(ctx, o)
}

func (t *Strategy) NewList() types.ObjectList {
	return t.translator.NewPublicList()
}

func (t *Strategy) Delete(ctx context.Context, obj types.Object) (types.Object, error) {
	newObj, err := t.fromPublic(ctx, obj)
	if err != nil {
		return nil, err
	}
	o, err := t.strategy.Delete(ctx, newObj)
	deletedObj, err := t.toPublic(ctx, o, err, obj.GetNamespace(), obj.GetName())
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return deletedObj, err
}

func (t *Strategy) translateListOpts(ctx context.Context, namespace string, opts storage.ListOptions) (string, storage.ListOptions, error) {
	if opts.Predicate.Field != nil {
		var err error
		opts.Predicate.Field, err = opts.Predicate.Field.Transform(func(field, value string) (newField, newValue string, err error) {
			if field == "metadata.name" {
				_, newName, err := t.translator.FromPublicName(ctx, namespace, value)
				if err != nil {
					return "", "", err
				}
				return field, newName, nil
			}
			return field, value, nil
		})
		if err != nil {
			return "", storage.ListOptions{}, err
		}
	}

	return t.translator.ListOpts(ctx, namespace, opts)
}

func (t *Strategy) Watch(ctx context.Context, namespace string, opts storage.ListOptions) (<-chan watch.Event, error) {
	namespace, newOpts, err := t.translateListOpts(ctx, namespace, opts)
	if err != nil {
		return nil, err
	}

	w, err := t.strategy.Watch(ctx, namespace, newOpts)
	if err != nil {
		return nil, err
	}

	result := make(chan watch.Event)
	go func() {
		defer close(result)

		for event := range w {
			switch event.Type {
			case watch.Bookmark:
				newObj := t.translator.NewPublic()
				m, err := meta.Accessor(event.Object)
				if err == nil {
					newObj.SetResourceVersion(m.GetResourceVersion())
					event.Object = newObj
					result <- event
				}
			case watch.Added:
				fallthrough
			case watch.Deleted:
				fallthrough
			case watch.Modified:
				objs, err := t.toPublicObjects(ctx, event.Object)
				if err != nil {
					result <- watch.Event{
						Type:   watch.Error,
						Object: &apierrors.NewInternalError(err).ErrStatus,
					}
					continue
				}

				for _, obj := range objs {
					if ok, err := opts.Predicate.Matches(obj); err != nil {
						result <- watch.Event{
							Type:   watch.Error,
							Object: &apierrors.NewInternalError(err).ErrStatus,
						}
					} else if ok {
						event.Object = obj
						result <- event
					}
				}
			default:
				result <- event
			}
		}
	}()

	return result, nil
}

func (t *Strategy) Destroy() {
	t.strategy.Destroy()
}

func (t *Strategy) Scheme() *runtime.Scheme {
	return t.strategy.Scheme()
}

func guessPluralName(name string) string {
	if name == "" {
		return name
	}

	if strings.EqualFold(name, "Endpoints") {
		return name
	}

	if suffix(name, "s") || suffix(name, "ch") || suffix(name, "x") || suffix(name, "sh") {
		return name + "es"
	}

	if suffix(name, "f") || suffix(name, "fe") {
		return name + "ves"
	}

	if suffix(name, "y") && len(name) > 2 && !strings.ContainsAny(name[len(name)-2:len(name)-1], "[aeiou]") {
		return name[0:len(name)-1] + "ies"
	}

	return name + "s"
}

func suffix(str, end string) bool {
	return strings.HasSuffix(str, end)
}
