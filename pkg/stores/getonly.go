package stores

import (
	"github.com/hobbyfarm/mink/pkg/strategy"
	"k8s.io/apiserver/pkg/registry/rest"
)

var (
	_ rest.Getter   = (*GetOnlyStore)(nil)
	_ strategy.Base = (*GetOnlyStore)(nil)
)

type GetOnlyStore struct {
	*strategy.SingularNameAdapter
	*strategy.GetAdapter
	*strategy.NewAdapter
	*strategy.DestroyAdapter
	*strategy.ScoperAdapter
	*strategy.TableAdapter
}
