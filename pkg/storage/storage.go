package storage

import (
	"fmt"

	applabels "github.com/redhat-developer/odo/pkg/application/labels"
	componentlabels "github.com/redhat-developer/odo/pkg/component/labels"
	"github.com/redhat-developer/odo/pkg/occlient"
	storagelabels "github.com/redhat-developer/odo/pkg/storage/labels"
	"github.com/redhat-developer/odo/pkg/util"

	corev1 "k8s.io/api/core/v1"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type StorageInfo struct {
	Name string
	Size string
	// if path is empty, it indicates that the storage is not mounted in any component
	Path string
}

// Create adds storage to given component of given application
func Create(client *occlient.Client, name string, size string, path string, componentName string, applicationName string) (string, error) {

	// Namespace the component
	// We will use name+applicationName instead of componentName+applicationName until:
	// https://github.com/redhat-developer/odo/issues/504 is resolved.
	namespacedOpenShiftObject, err := util.NamespaceOpenShiftObject(name, applicationName)
	if err != nil {
		return "", errors.Wrapf(err, "unable to create namespaced name")
	}

	labels := storagelabels.GetLabels(name, componentName, applicationName, true)

	log.Debugf("Got labels for PVC: %v", labels)

	// Create PVC
	pvc, err := client.CreatePVC(generatePVCNameFromStorageName(namespacedOpenShiftObject), size, labels)
	if err != nil {
		return "", errors.Wrap(err, "unable to create PVC")
	}

	// Get DeploymentConfig for the given component
	componentLabels := componentlabels.GetLabels(componentName, applicationName, false)
	componentSelector := util.ConvertLabelsToSelector(componentLabels)
	dc, err := client.GetOneDeploymentConfigFromSelector(componentSelector)
	if err != nil {
		return "", errors.Wrapf(err, "unable to get Deployment Config for component: %v in application: %v", componentName, applicationName)
	}
	log.Debugf("Deployment Config: %v is associated with the component: %v", dc.Name, componentName)

	// Add PVC to DeploymentConfig
	if err := client.AddPVCToDeploymentConfig(dc, pvc.Name, path); err != nil {
		return "", errors.Wrap(err, "unable to add PVC to DeploymentConfig")
	}

	return dc.Name, nil
}

// Unmount unmounts the given storage from the given component
func Unmount(client *occlient.Client, storageName string, componentName string, applicationName string) error {
	// Get DeploymentConfig for the given component
	componentLabels := componentlabels.GetLabels(componentName, applicationName, false)
	componentSelector := util.ConvertLabelsToSelector(componentLabels)
	dc, err := client.GetOneDeploymentConfigFromSelector(componentSelector)
	if err != nil {
		return errors.Wrapf(err, "unable to get Deployment Config for component: %v in application: %v", componentName, applicationName)
	}

	pvcName, err := getPVCNameFromStorageName(client, storageName)
	if err != nil {
		return errors.Wrapf(err, "unable to get PVC for storage %v", storageName)
	}

	// Remove PVC from Deployment Config
	if err := client.RemoveVolumeFromDeploymentConfig(pvcName, dc.Name); err != nil {
		return errors.Wrapf(err, "unable to remove volume: %v from Deployment Config: %v", pvcName, dc.Name)
	}

	pvc, err := client.GetPVCFromName(pvcName)
	pvcLabels := applabels.GetLabels(applicationName, true)
	pvcLabels[storagelabels.StorageLabel] = storageName

	if err := client.UpdatePVCLabels(pvc, pvcLabels); err != nil {
		return errors.Wrapf(err, "unable to remove storage label from : %v", pvc.Name)
	}
	return nil
}

// Delete removes storage from the given application.
// Delete returns the component name, if it is mounted to a component, or "" and the error, if any
func Delete(client *occlient.Client, name string, applicationName string) (string, error) {
	// unmount the storage from the component if mounted
	componentName, err := GetComponentNameFromStorageName(client, name)
	if err != nil {
		return "", errors.Wrap(err, "unable to find component name and app name")
	}
	if componentName != "" {
		err := Unmount(client, name, componentName, applicationName)
		if err != nil {
			return "", errors.Wrapf(err, "unable to unmount storage %v", name)
		}
	}

	pvcName, err := getPVCNameFromStorageName(client, name)
	if err != nil {
		return "", errors.Wrapf(err, "unable to get PVC for storage %v", name)
	}

	// delete the associated PVC with the component
	err = client.DeletePVC(pvcName)
	if err != nil {
		return "", errors.Wrapf(err, "unable to delete PVC %v", pvcName)
	}

	return componentName, nil
}

// List lists all the mounted storage associated with the given component of the given
// application and the unmounted storages in the given application
func List(client *occlient.Client, componentName string, applicationName string) ([]StorageInfo, error) {
	componentLabels := componentlabels.GetLabels(componentName, applicationName, false)
	componentSelector := util.ConvertLabelsToSelector(componentLabels)

	dc, err := client.GetOneDeploymentConfigFromSelector(componentSelector)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get Deployment Config associated with component %v", componentName)
	}

	// store the storages in a map for faster searching with the key instead of list
	mountedStorageMap := make(map[string]string)
	volumeMounts := client.GetVolumeMountsFromDC(dc)
	for _, volumeMount := range volumeMounts {
		pvcName := client.GetPVCNameFromVolumeMountName(volumeMount.Name, dc)
		if pvcName == "" {
			return nil, fmt.Errorf("no PVC associated with Volume Mount %v", volumeMount.Name)
		}
		pvc, err := client.GetPVCFromName(pvcName)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to get PVC %v", pvcName)
		}

		storageName := getStorageFromPVC(pvc)
		mountedStorageMap[storageName] = volumeMount.MountPath
	}

	pvcs, err := client.GetPVCsFromSelector(storagelabels.StorageLabel)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get Deployment Config associated with component %v", componentName)
	}
	var storageList []StorageInfo
	for _, pvc := range pvcs {
		pvcComponentName, ok := pvc.Labels[componentlabels.ComponentLabel]
		pvcAppName, okApp := pvc.Labels[applabels.ApplicationLabel]
		// first check if storage label does not exists indicating that the storage is not mounted in any component
		// if the storage label exists, then check if the component is the current active component
		// also check if the app label exists and is equal to the current application
		if (!ok || pvcComponentName == componentName) && (okApp && pvcAppName == applicationName) {
			if pvc.Name == "" {
				return nil, fmt.Errorf("no PVC associated")
			}
			storageName := getStorageFromPVC(&pvc)
			storageSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			storagePath := mountedStorageMap[storageName]
			storageList = append(storageList, StorageInfo{
				Name: storageName,
				Size: storageSize.String(),
				Path: storagePath,
			})
		}
	}
	return storageList, nil
}

// Exists checks if the given storage exists in the given application
func Exists(client *occlient.Client, storageName string, applicationName string) (bool, error) {
	var labels = make(map[string]string)
	labels[applabels.ApplicationLabel] = applicationName
	labels[storagelabels.StorageLabel] = storageName
	selector := util.ConvertLabelsToSelector(labels)
	pvcs, err := client.GetPVCsFromSelector(selector)
	if err != nil {
		return false, errors.Wrapf(err, "unable to list storage for application %v", applicationName)
	}

	if len(pvcs) <= 0 {
		return false, nil
	}
	return true, nil
}

// generatePVCNameFromStorageName generates a PVC name from the given storage
// name
func generatePVCNameFromStorageName(storage string) string {
	return fmt.Sprintf("%v-pvc", storage)
}

// getStorageFromPVC returns the storage associated with the given PVC
func getStorageFromPVC(pvc *corev1.PersistentVolumeClaim) string {
	if _, ok := pvc.Labels[storagelabels.StorageLabel]; !ok {
		return ""
	}
	return pvc.Labels[storagelabels.StorageLabel]
}

// getPVCNameFromStorageName returns the PVC associated with the given storage
func getPVCNameFromStorageName(client *occlient.Client, storageName string) (string, error) {
	var labels = make(map[string]string)
	labels[storagelabels.StorageLabel] = storageName

	selector := util.ConvertLabelsToSelector(labels)
	pvcs, err := client.GetPVCNamesFromSelector(selector)
	if err != nil {
		return "", errors.Wrapf(err, "unable to get PVC names for selector %v", selector)
	}
	numPVCs := len(pvcs)
	if numPVCs != 1 {
		return "", fmt.Errorf("expected exactly one PVC attached to storage %v, but got %v, %v", storageName, numPVCs, pvcs)
	}
	return pvcs[0], nil
}

// GetComponentNameFromStorageName returns the component name associated with the storageName, if any, or ""
func GetComponentNameFromStorageName(client *occlient.Client, storageName string) (string, error) {
	var labels = make(map[string]string)
	labels[storagelabels.StorageLabel] = storageName

	selector := util.ConvertLabelsToSelector(labels)
	pvcs, err := client.GetPVCsFromSelector(selector)
	if err != nil {
		return "", errors.Wrap(err, "unable to list the pvcs")
	}
	if len(pvcs) > 1 {
		return "", errors.Wrap(err, "more than one pvc found for the storage label")
	}
	if len(pvcs) == 1 {
		pvc := pvcs[0]
		labels = pvc.GetLabels()
		return labels[componentlabels.ComponentLabel], nil
	}
	return "", nil
}

// IsMounted checks if the given storage is mounted to the given component
// IsMounted returns a bool indicating the storage is mounted to the component or not
func IsMounted(client *occlient.Client, storageName string, componentName string, applicationName string) (bool, error) {
	storageList, err := List(client, componentName, applicationName)
	if err != nil {
		return false, errors.Wrapf(err, "unable to list storage for component %v", componentName)
	}
	for _, storage := range storageList {
		if storage.Name == storageName {
			if storage.Path != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

//GetMountPath returns mount path for given storage
func GetMountPath(client *occlient.Client, storageName string, componentName string, applicationName string) string {
	var mPath string
	storageInfo, _ := List(client, componentName, applicationName)
	for _, storage := range storageInfo {
		if storage.Name == storageName {
			mPath = storage.Path
		}
	}
	return mPath
}

// Mount mounts the given storage to the given component
func Mount(client *occlient.Client, path string, storageName string, componentName string, applicationName string) error {
	storageComponent, err := GetComponentNameFromStorageName(client, storageName)
	if err != nil {
		return errors.Wrap(err, "unable to get the component name associated with the storage")
	}
	if storageComponent != "" {
		return fmt.Errorf("the given storage is already mounted to the component %v", storageComponent)
	}

	namespacedOpenShiftObject, err := util.NamespaceOpenShiftObject(storageName, applicationName)
	if err != nil {
		return errors.Wrapf(err, "unable to create namespaced name")
	}

	pvc, err := client.GetPVCFromName(generatePVCNameFromStorageName(namespacedOpenShiftObject))
	if err != nil {
		return errors.Wrap(err, "unable to get the pvc from the storage name")
	}

	// Get DeploymentConfig for the given component
	componentLabels := componentlabels.GetLabels(componentName, applicationName, false)
	componentSelector := util.ConvertLabelsToSelector(componentLabels)
	dc, err := client.GetOneDeploymentConfigFromSelector(componentSelector)
	if err != nil {
		return errors.Wrapf(err, "unable to get Deployment Config for component: %v in application: %v", componentName, applicationName)
	}
	log.Debugf("Deployment Config: %v is associated with the component: %v", dc.Name, componentName)

	// Add PVC to DeploymentConfig
	if err := client.AddPVCToDeploymentConfig(dc, pvc.Name, path); err != nil {
		return errors.Wrap(err, "unable to add PVC to DeploymentConfig")
	}
	err = client.UpdatePVCLabels(pvc, storagelabels.GetLabels(storageName, componentName, applicationName, true))
	if err != nil {
		return errors.Wrap(err, "unable to update the pvc")
	}
	return nil
}

// Gets the storageName mounted to the given path in the given component and application
// GetStorageNameFromMountPath returns the name of the storage or the error
func GetStorageNameFromMountPath(client *occlient.Client, path string, componentName string, applicationName string) (string, error) {
	storages, err := List(client, componentName, applicationName)
	if err != nil {
		return "", errors.Wrapf(err, "unable to list storage for component %v", componentName)
	}
	for _, storage := range storages {
		if storage.Path == path {
			return storage.Name, nil
		}
	}
	return "", nil
}
