/*
 *  Copyright (c) 2023 Juice Technologies, Inc. All Rights Reserved.
 */
package gpu

import (
	"fmt"
	"os/exec"

	"github.com/Xdevlab/Run/pkg/gpu"
)

func DetectGpus(rendererWinPath string) (*gpu.GpuSet, error) {
	cmd := exec.Command(rendererWinPath,
		"--log_group", "Fatal",
		"--dump_gpus", "0")
	output, err := cmd.Output()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("DetectGpus: Renderer_Win failed with %s, %s", err, exiterr.Stderr)
		} else {
			return nil, fmt.Errorf("DetectGpus: Renderer_Win failed with %s", err)
		}

	}

	if cmd.ProcessState.ExitCode() == 0 {
		return gpu.NewGpuSetFromJson(output)
	}

	return nil, fmt.Errorf("DetectGpus: Renderer_Win exited with %d", cmd.ProcessState.ExitCode())
}
