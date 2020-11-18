package csi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/stretchr/testify/assert"
)

const iterations = 25
const interval = 100 * time.Millisecond

func NewFakeManager() *volumes {
	ctx, cancel := context.WithCancel(context.Background())
	return &volumes{
		m:                make(map[string]*api.VolumeAssignment),
		plugins:          newFakePluginManager(),
		tryVolumesCtx:    ctx,
		tryVolumesCancel: cancel,
	}
}

func TestTaskRestrictedVolumesProvider(t *testing.T) {
	driver := "driver"

	volumesManager := NewFakeManager()
	volumesManager.plugins.Set([]*api.CSINodePlugin{{Name: driver}})

	taskID := "taskID1"
	type testCase struct {
		desc        string
		volumes     exec.VolumeGetter
		volumeID    string
		expectedErr string
	}

	testCases := []testCase{
		// The default case when not using a volumes driver or not returning.
		// Test to check if volume ID is allowed to access
		{
			desc:     "AllowedVolume",
			volumeID: "volume1",
		},
		// Test to check if volume ID is not allowed to access
		{
			desc:        "RestrictedVolume",
			expectedErr: fmt.Sprintf("task not authorized to access volume volume2"),
			volumeID:    "volume2",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.desc, func(t *testing.T) {
			v := &api.VolumeAssignment{
				ID:     testCase.volumeID,
				Driver: &api.Driver{Name: driver},
			}
			// adding to the map happens in Add, not in tryAdd, so we do that
			// manually
			volumesManager.m[testCase.volumeID] = v
			// call tryAddVolume in this test so that the add happens synchronously
			volumesManager.tryAddVolume(context.Background(), v)
			volumesGetter := Restrict(volumesManager, &api.Task{
				ID: taskID,
				Volumes: []*api.VolumeAttachment{
					{
						ID: "volume1",
					},
				},
			})

			volume, err := volumesGetter.Get(testCase.volumeID)
			if testCase.expectedErr != "" {
				assert.Error(t, err, testCase.desc)
				assert.Equal(t, testCase.expectedErr, err.Error())
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, volume)
			}
			volumesManager.Reset()
		})
	}
}