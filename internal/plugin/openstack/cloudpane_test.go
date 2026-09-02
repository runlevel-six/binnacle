package openstack

import (
	"errors"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestNotDeployedRecognisesAMissingService(t *testing.T) {
	// Octavia absent from the catalog is what this looks like in practice.
	if !notDeployed(gophercloud.ErrEndpointNotFound{}) {
		t.Error("ErrEndpointNotFound not recognized as an absent service")
	}
	if !notDeployed(errors.Join(errors.New("wrapped"), gophercloud.ErrEndpointNotFound{})) {
		t.Error("wrapped ErrEndpointNotFound not recognized")
	}
	if notDeployed(errors.New("connection refused")) {
		t.Error("an ordinary failure was reported as an absent service")
	}
	if notDeployed(nil) {
		t.Error("nil reported as an absent service")
	}
}
