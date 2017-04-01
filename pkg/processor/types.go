package processor

import (
	"github.com/atlassian/smith"

	"github.com/cenk/backoff"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type ReadyChecker interface {
	IsReady(*unstructured.Unstructured) (isReady, retriableError bool, e error)
}

type BackOffFactory func() backoff.BackOff

type bundleRef struct {
	namespace  string
	bundleName string
}

type workRequest struct {
	bundleRef
	work chan<- *smith.Bundle
}

type notifyRequest struct {
	bundleRef
	bundle *smith.Bundle
	notify chan<- struct{}
}
