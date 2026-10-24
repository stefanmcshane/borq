package apps

import "fmt"

func mergePredeploy(task Predeploy, update PredeployPatch) Predeploy {
	if update.Command != nil {
		task.Command = *update.Command
	}
	if update.TimeoutSeconds != nil {
		task.TimeoutSeconds = *update.TimeoutSeconds
	}
	if update.BackoffLimit != nil {
		task.BackoffLimit = *update.BackoffLimit
	}
	return task
}

func validatePredeploy(task Predeploy) error {
	if task.TimeoutSeconds < 1 || int64(task.TimeoutSeconds) > 2147483647 {
		return fmt.Errorf("predeploy timeoutSeconds must be between 1 and 2147483647")
	}
	if task.BackoffLimit < 0 || int64(task.BackoffLimit) > 2147483647 {
		return fmt.Errorf("predeploy backoffLimit must be between 0 and 2147483647")
	}
	return nil
}
