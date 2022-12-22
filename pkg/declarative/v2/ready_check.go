package v2

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kyma-project/module-manager/pkg/types"
	"github.com/kyma-project/module-manager/pkg/util"
	"helm.sh/helm/v3/pkg/kube"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var ErrResourcesNotReady = errors.New("resources are not ready")

type ReadyCheck interface {
	Run(ctx context.Context, resources []*resource.Info) error
}

type HelmReadyCheck struct {
	clientSet kubernetes.Interface
}

func NewHelmReadyCheck(factory kube.Factory) ReadyCheck {
	clientSet, _ := factory.KubernetesClientSet()
	return &HelmReadyCheck{clientSet: clientSet}
}

func NewExistsReadyCheck(client client.Reader) ReadyCheck {
	return &ExistsReadyCheck{Reader: client}
}

func (c *HelmReadyCheck) Run(ctx context.Context, resources []*resource.Info) error {
	start := time.Now()
	logger := log.FromContext(ctx)
	logger.V(util.TraceLogLevel).Info("ReadyCheck", "resources", len(resources))
	checker := kube.NewReadyChecker(
		c.clientSet, func(format string, args ...interface{}) {
			logger.V(util.DebugLogLevel).Info(fmt.Sprintf(format, args...))
		}, kube.PausedAsReady(false), kube.CheckJobs(true),
	)

	readyCheckResults := make(chan error, len(resources))

	isReady := func(ctx context.Context, i int) {
		ready, err := checker.IsReady(ctx, resources[i])
		if !ready {
			readyCheckResults <- ErrResourcesNotReady
		} else {
			readyCheckResults <- err
		}
	}

	for i := range resources {
		i := i
		go isReady(ctx, i)
	}

	var errs []error
	for i := 0; i < len(resources); i++ {
		err := <-readyCheckResults
		if errors.Is(err, ErrResourcesNotReady) {
			return err
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return types.NewMultiError(errs)
	}

	logger.V(util.DebugLogLevel).Info(
		"ReadyCheck finished",
		"resources", len(resources), "time", time.Since(start),
	)

	return nil
}

type ExistsReadyCheck struct {
	client.Reader
}

func (c *ExistsReadyCheck) Run(ctx context.Context, resources []*resource.Info) error {
	for i := range resources {
		obj, ok := resources[i].Object.(client.Object)
		if !ok {
			return errors.New("object in resource info is not a valid client object")
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
