package ingress

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	iov1 "github.com/openshift/api/operatoringress/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/util/retryableerror"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilclock "k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// clock is to enable unit testing
var clock utilclock.Clock = utilclock.RealClock{}

// syncIngressControllerStatus computes the current status of ic and
// updates status upon any changes since last sync.
func (r *reconciler) syncIngressControllerStatus(ic *operatorv1.IngressController, deployment *appsv1.Deployment, service *corev1.Service, operandEvents []corev1.Event, wildcardRecord *iov1.DNSRecord, dnsConfig *configv1.DNS) error {
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return fmt.Errorf("deployment has invalid spec.selector: %v", err)
	}

	var errs []error

	updated := ic.DeepCopy()
	updated.Status.AvailableReplicas = deployment.Status.AvailableReplicas
	updated.Status.Selector = selector.String()
	updated.Status.TLSProfile = computeIngressTLSProfile(ic.Status.TLSProfile, deployment)
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeIngressAvailableCondition(deployment))
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeDeploymentAvailableCondition(deployment))
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeDeploymentReplicasMinAvailableCondition(deployment))
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeDeploymentReplicasAllAvailableCondition(deployment))
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeLoadBalancerStatus(ic, service, operandEvents)...)
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, computeDNSStatus(ic, wildcardRecord, dnsConfig)...)
	degradedCondition, err := computeIngressDegradedCondition(updated.Status.Conditions)
	errs = append(errs, err)
	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, degradedCondition)

	if !ingressStatusesEqual(updated.Status, ic.Status) {
		if err := r.client.Status().Update(context.TODO(), updated); err != nil {
			errs = append(errs, fmt.Errorf("failed to update ingresscontroller status: %v", err))
		}
	}

	return retryableerror.NewMaybeRetryableAggregate(errs)
}

// mergeConditions adds or updates matching conditions, and updates
// the transition time if details of a condition have changed. Returns
// the updated condition array.
func mergeConditions(conditions []operatorv1.OperatorCondition, updates ...operatorv1.OperatorCondition) []operatorv1.OperatorCondition {
	now := metav1.NewTime(clock.Now())
	var additions []operatorv1.OperatorCondition
	for i, update := range updates {
		add := true
		for j, cond := range conditions {
			if cond.Type == update.Type {
				add = false
				if conditionChanged(cond, update) {
					conditions[j].Status = update.Status
					conditions[j].Reason = update.Reason
					conditions[j].Message = update.Message
					conditions[j].LastTransitionTime = now
					break
				}
			}
		}
		if add {
			updates[i].LastTransitionTime = now
			additions = append(additions, updates[i])
		}
	}
	conditions = append(conditions, additions...)
	return conditions
}

// computeIngressTLSProfile computes the ingresscontroller's current TLS
// profile.  If the deployment is ready, then the TLS profile is inferred from
// deployment's pod template spec.  Otherwise the previous TLS profile is used.
func computeIngressTLSProfile(oldProfile *configv1.TLSProfileSpec, deployment *appsv1.Deployment) *configv1.TLSProfileSpec {
	if deployment.Status.Replicas != deployment.Status.UpdatedReplicas {
		return oldProfile
	}

	newProfile := inferTLSProfileSpecFromDeployment(deployment)

	return newProfile
}

// computeIngressAvailableCondition computes the ingress controller's current Available status state
// by inspecting the Available condition of deployment. The ingresscontroller is only available if
// the deployment is also available.
func computeIngressAvailableCondition(deployment *appsv1.Deployment) operatorv1.OperatorCondition {
	for _, cond := range deployment.Status.Conditions {
		if cond.Type != appsv1.DeploymentAvailable {
			continue
		}
		switch cond.Status {
		case corev1.ConditionTrue:
			return operatorv1.OperatorCondition{
				Type:   operatorv1.IngressControllerAvailableConditionType,
				Status: operatorv1.ConditionTrue,
			}
		case corev1.ConditionFalse:
			return operatorv1.OperatorCondition{
				Type:    operatorv1.IngressControllerAvailableConditionType,
				Status:  operatorv1.ConditionFalse,
				Reason:  cond.Reason,
				Message: "The deployment is unavailable: " + cond.Message,
			}
		}
	}

	return operatorv1.OperatorCondition{
		Type:    operatorv1.IngressControllerAvailableConditionType,
		Status:  operatorv1.ConditionFalse,
		Reason:  "DeploymentAvailabilityUnknown",
		Message: "The deployment's Available condition couldn't be interpreted",
	}
}

// computeDeploymentAvailableCondition computes the ingresscontroller's
// "DeploymentAvailable" status condition by examining the status conditions of
// the deployment.  The "DeploymentAvailable" condition is true if the
// deployment's "Available" condition is true.
//
// Note: Due to a defect in the deployment controller, the deployment reports
// Available=True before minimum availability requirements are met (see
// <https://bugzilla.redhat.com/show_bug.cgi?id=1830271#c5>).
func computeDeploymentAvailableCondition(deployment *appsv1.Deployment) operatorv1.OperatorCondition {
	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable {
			switch cond.Status {
			case corev1.ConditionFalse:
				return operatorv1.OperatorCondition{
					Type:    IngressControllerDeploymentAvailableConditionType,
					Status:  operatorv1.ConditionFalse,
					Reason:  "DeploymentUnavailable",
					Message: fmt.Sprintf("The deployment has Available status condition set to False (reason: %s) with message: %s", cond.Reason, cond.Message),
				}
			case corev1.ConditionTrue:
				return operatorv1.OperatorCondition{
					Type:    IngressControllerDeploymentAvailableConditionType,
					Status:  operatorv1.ConditionTrue,
					Reason:  "DeploymentAvailable",
					Message: "The deployment has Available status condition set to True",
				}
			}
			break
		}
	}
	return operatorv1.OperatorCondition{
		Type:    IngressControllerDeploymentAvailableConditionType,
		Status:  operatorv1.ConditionUnknown,
		Reason:  "DeploymentAvailabilityUnknown",
		Message: "The deployment has no Available status condition set",
	}
}

// computeDeploymentReplicasMinAvailableCondition computes the
// ingresscontroller's "DeploymentReplicasMinAvailable" status condition by
// examining the number of available replicas reported in the deployment's
// status and the maximum unavailable as configured in the deployment's rolling
// update parameters.  The "DeploymentReplicasMinAvailable" condition is true if
// the number of available replicas is equal to or greater than the number of
// desired replicas less the number minimum available.
func computeDeploymentReplicasMinAvailableCondition(deployment *appsv1.Deployment) operatorv1.OperatorCondition {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	pointerTo := func(val intstr.IntOrString) *intstr.IntOrString { return &val }
	maxUnavailableIntStr := pointerTo(intstr.FromString("25%"))
	maxSurgeIntStr := pointerTo(intstr.FromString("25%"))
	if deployment.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && deployment.Spec.Strategy.RollingUpdate != nil {
		if deployment.Spec.Strategy.RollingUpdate.MaxUnavailable != nil {
			maxUnavailableIntStr = deployment.Spec.Strategy.RollingUpdate.MaxUnavailable
		}
		if deployment.Spec.Strategy.RollingUpdate.MaxSurge != nil {
			maxSurgeIntStr = deployment.Spec.Strategy.RollingUpdate.MaxSurge
		}
	}
	maxSurge, err := intstr.GetValueFromIntOrPercent(maxSurgeIntStr, int(replicas), true)
	if err != nil {
		return operatorv1.OperatorCondition{
			Type:    IngressControllerDeploymentReplicasMinAvailableConditionType,
			Status:  operatorv1.ConditionUnknown,
			Reason:  "InvalidMaxSurgeValue",
			Message: fmt.Sprintf("invalid value for max surge: %v", err),
		}
	}
	maxUnavailable, err := intstr.GetValueFromIntOrPercent(maxUnavailableIntStr, int(replicas), false)
	if err != nil {
		return operatorv1.OperatorCondition{
			Type:    IngressControllerDeploymentReplicasMinAvailableConditionType,
			Status:  operatorv1.ConditionUnknown,
			Reason:  "InvalidMaxUnavailableValue",
			Message: fmt.Sprintf("invalid value for max unavailable: %v", err),
		}
	}
	if maxSurge == 0 && maxUnavailable == 0 {
		maxUnavailable = 1
	}
	if int(deployment.Status.AvailableReplicas) < int(replicas)-maxUnavailable {
		return operatorv1.OperatorCondition{
			Type:    IngressControllerDeploymentReplicasMinAvailableConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "DeploymentMinimumReplicasNotMet",
			Message: fmt.Sprintf("%d/%d of replicas are available, max unavailable is %d", deployment.Status.AvailableReplicas, replicas, maxUnavailable),
		}
	}

	return operatorv1.OperatorCondition{
		Type:    IngressControllerDeploymentReplicasMinAvailableConditionType,
		Status:  operatorv1.ConditionTrue,
		Reason:  "DeploymentMinimumReplicasMet",
		Message: "Minimum replicas requirement is met",
	}
}

// computeDeploymentReplicasAllAvailableCondition computes the
// ingresscontroller's "DeploymentReplicasAllAvailable" status condition by
// examining the number of available replicas reported in the deployment's
// status.  The "DeploymentReplicasAllAvailable" condition is true if the number
// of available replicas is equal to or greater than the number of desired
// replicas.
func computeDeploymentReplicasAllAvailableCondition(deployment *appsv1.Deployment) operatorv1.OperatorCondition {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	if deployment.Status.AvailableReplicas < replicas {
		return operatorv1.OperatorCondition{
			Type:    IngressControllerDeploymentReplicasAllAvailableConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "DeploymentReplicasNotAvailable",
			Message: fmt.Sprintf("%d/%d of replicas are available", deployment.Status.AvailableReplicas, replicas),
		}
	}

	return operatorv1.OperatorCondition{
		Type:    IngressControllerDeploymentReplicasAllAvailableConditionType,
		Status:  operatorv1.ConditionTrue,
		Reason:  "DeploymentReplicasAvailable",
		Message: "All replicas are available",
	}
}

// computeIngressDegradedCondition computes the ingresscontroller's "Degraded"
// status condition, which aggregates other status conditions that can indicate
// a degraded state.  In addition, computeIngressDegradedCondition returns a
// duration value that indicates, if it is non-zero, that the operator should
// reconcile the ingresscontroller again after that period to update its status
// conditions.
func computeIngressDegradedCondition(conditions []operatorv1.OperatorCondition) (operatorv1.OperatorCondition, error) {
	var requeueAfter time.Duration
	conditionsMap := make(map[string]*operatorv1.OperatorCondition)
	for i := range conditions {
		conditionsMap[conditions[i].Type] = &conditions[i]
	}

	expectedConditions := []struct {
		condition        string
		status           operatorv1.ConditionStatus
		ifConditionsTrue []string
		gracePeriod      time.Duration
	}{
		{
			condition: IngressControllerAdmittedConditionType,
			status:    operatorv1.ConditionTrue,
		},
		{
			condition:   IngressControllerDeploymentAvailableConditionType,
			status:      operatorv1.ConditionTrue,
			gracePeriod: time.Second * 30,
		},
		{
			condition:   IngressControllerDeploymentReplicasMinAvailableConditionType,
			status:      operatorv1.ConditionTrue,
			gracePeriod: time.Second * 60,
		},
		{
			condition:   IngressControllerDeploymentReplicasAllAvailableConditionType,
			status:      operatorv1.ConditionTrue,
			gracePeriod: time.Minute * 60,
		},
		{
			condition:        operatorv1.LoadBalancerReadyIngressConditionType,
			status:           operatorv1.ConditionTrue,
			ifConditionsTrue: []string{operatorv1.LoadBalancerManagedIngressConditionType},
			gracePeriod:      time.Second * 90,
		},
		{
			condition: operatorv1.DNSReadyIngressConditionType,
			status:    operatorv1.ConditionTrue,
			ifConditionsTrue: []string{
				operatorv1.LoadBalancerManagedIngressConditionType,
				operatorv1.LoadBalancerReadyIngressConditionType,
				operatorv1.DNSManagedIngressConditionType,
			},
			gracePeriod: time.Second * 30,
		},
	}

	var graceConditions, degradedConditions []*operatorv1.OperatorCondition
	now := clock.Now()
	for _, expected := range expectedConditions {
		condition, haveCondition := conditionsMap[expected.condition]
		if !haveCondition {
			continue
		}
		if condition.Status == expected.status {
			continue
		}
		failedPredicates := false
		for _, ifCond := range expected.ifConditionsTrue {
			predicate, havePredicate := conditionsMap[ifCond]
			if !havePredicate || predicate.Status != operatorv1.ConditionTrue {
				failedPredicates = true
				break
			}
		}
		if failedPredicates {
			continue
		}
		if expected.gracePeriod != 0 {
			t1 := now.Add(-expected.gracePeriod)
			t2 := condition.LastTransitionTime
			if t2.After(t1) {
				d := t2.Sub(t1)
				if len(graceConditions) == 0 || d < requeueAfter {
					// Recompute status conditions again
					// after the grace period has elapsed.
					requeueAfter = d
				}
				graceConditions = append(graceConditions, condition)
				continue
			}
		}
		degradedConditions = append(degradedConditions, condition)
	}

	if len(degradedConditions) != 0 {
		// Keep checking conditions every minute while degraded.
		requeueAfter = time.Minute

		var degraded string
		for _, cond := range degradedConditions {
			degraded = degraded + fmt.Sprintf(", %s=%s", cond.Type, cond.Status)
		}
		degraded = degraded[2:]

		condition := operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeDegraded,
			Status:  operatorv1.ConditionTrue,
			Reason:  "DegradedConditions",
			Message: "One or more other status conditions indicate a degraded state: " + degraded,
		}

		return condition, retryableerror.New(errors.New("IngressController is degraded: "+degraded), requeueAfter)
	}

	condition := operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeDegraded,
		Status: operatorv1.ConditionFalse,
	}

	var err error
	if len(graceConditions) != 0 {
		var grace string
		for _, cond := range graceConditions {
			grace = grace + fmt.Sprintf(", %s=%s", cond.Type, cond.Status)
		}
		grace = grace[2:]

		err = retryableerror.New(errors.New("IngressController may become degraded soon: "+grace), requeueAfter)
	}

	return condition, err
}

// ingressStatusesEqual compares two IngressControllerStatus values.  Returns true
// if the provided values should be considered equal for the purpose of determining
// whether an update is necessary, false otherwise.
func ingressStatusesEqual(a, b operatorv1.IngressControllerStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if !conditionsEqual(a.Conditions, b.Conditions) || a.AvailableReplicas != b.AvailableReplicas ||
		a.Selector != b.Selector {
		return false
	}
	if !reflect.DeepEqual(a.TLSProfile, b.TLSProfile) {
		return false
	}

	return true
}

func conditionsEqual(a, b []operatorv1.OperatorCondition) bool {
	conditionCmpOpts := []cmp.Option{
		cmpopts.EquateEmpty(),
		cmpopts.SortSlices(func(a, b operatorv1.OperatorCondition) bool { return a.Type < b.Type }),
	}
	if !cmp.Equal(a, b, conditionCmpOpts...) {
		return false
	}
	return true
}

func conditionChanged(a, b operatorv1.OperatorCondition) bool {
	return a.Status != b.Status || a.Reason != b.Reason || a.Message != b.Message
}

// computeLoadBalancerStatus returns the complete set of current
// LoadBalancer-prefixed conditions for the given ingress controller.
func computeLoadBalancerStatus(ic *operatorv1.IngressController, service *corev1.Service, operandEvents []corev1.Event) []operatorv1.OperatorCondition {
	if ic.Status.EndpointPublishingStrategy == nil ||
		ic.Status.EndpointPublishingStrategy.Type != operatorv1.LoadBalancerServiceStrategyType {
		return []operatorv1.OperatorCondition{
			{
				Type:    operatorv1.LoadBalancerManagedIngressConditionType,
				Status:  operatorv1.ConditionFalse,
				Reason:  "EndpointPublishingStrategyExcludesManagedLoadBalancer",
				Message: fmt.Sprintf("The configured endpoint publishing strategy does not include a managed load balancer"),
			},
		}
	}

	conditions := []operatorv1.OperatorCondition{}

	conditions = append(conditions, operatorv1.OperatorCondition{
		Type:    operatorv1.LoadBalancerManagedIngressConditionType,
		Status:  operatorv1.ConditionTrue,
		Reason:  "WantedByEndpointPublishingStrategy",
		Message: "The endpoint publishing strategy supports a managed load balancer",
	})

	switch {
	case service == nil:
		conditions = append(conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.LoadBalancerReadyIngressConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "ServiceNotFound",
			Message: "The LoadBalancer service resource is missing",
		})
	case isProvisioned(service):
		conditions = append(conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.LoadBalancerReadyIngressConditionType,
			Status:  operatorv1.ConditionTrue,
			Reason:  "LoadBalancerProvisioned",
			Message: "The LoadBalancer service is provisioned",
		})
	case isPending(service):
		reason := "LoadBalancerPending"
		message := "The LoadBalancer service is pending"

		// Try and find a more specific reason for for the pending status.
		createFailedReason := "SyncLoadBalancerFailed"
		failedLoadBalancerEvents := getEventsByReason(operandEvents, "service-controller", createFailedReason)
		for _, event := range failedLoadBalancerEvents {
			involved := event.InvolvedObject
			if involved.Kind == "Service" && involved.Namespace == service.Namespace && involved.Name == service.Name && involved.UID == service.UID {
				reason = "SyncLoadBalancerFailed"
				message = fmt.Sprintf("The %s component is reporting SyncLoadBalancerFailed events like: %s\n%s",
					event.Source.Component, event.Message, "The kube-controller-manager logs may contain more details.")
				break
			}
		}
		conditions = append(conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.LoadBalancerReadyIngressConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
	}

	return conditions
}

func isProvisioned(service *corev1.Service) bool {
	ingresses := service.Status.LoadBalancer.Ingress
	return len(ingresses) > 0 && (len(ingresses[0].Hostname) > 0 || len(ingresses[0].IP) > 0)
}

func isPending(service *corev1.Service) bool {
	return !isProvisioned(service)
}

func getEventsByReason(events []corev1.Event, component, reason string) []corev1.Event {
	var filtered []corev1.Event
	for i := range events {
		event := events[i]
		if event.Source.Component == component && event.Reason == reason {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func computeDNSStatus(ic *operatorv1.IngressController, wildcardRecord *iov1.DNSRecord, dnsConfig *configv1.DNS) []operatorv1.OperatorCondition {
	if dnsConfig.Spec.PublicZone == nil && dnsConfig.Spec.PrivateZone == nil {
		return []operatorv1.OperatorCondition{
			{
				Type:    operatorv1.DNSManagedIngressConditionType,
				Status:  operatorv1.ConditionFalse,
				Reason:  "NoDNSZones",
				Message: "No DNS zones are defined in the cluster dns config.",
			},
		}
	}

	if ic.Status.EndpointPublishingStrategy.Type != operatorv1.LoadBalancerServiceStrategyType {
		return []operatorv1.OperatorCondition{
			{
				Type:    operatorv1.DNSManagedIngressConditionType,
				Status:  operatorv1.ConditionFalse,
				Reason:  "UnsupportedEndpointPublishingStrategy",
				Message: "The endpoint publishing strategy doesn't support DNS management.",
			},
		}
	}

	conditions := []operatorv1.OperatorCondition{
		{
			Type:    operatorv1.DNSManagedIngressConditionType,
			Status:  operatorv1.ConditionTrue,
			Reason:  "Normal",
			Message: "DNS management is supported and zones are specified in the cluster DNS config.",
		},
	}

	switch {
	case wildcardRecord == nil:
		conditions = append(conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.DNSReadyIngressConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "RecordNotFound",
			Message: "The wildcard record resource was not found.",
		})
	case len(wildcardRecord.Status.Zones) == 0:
		conditions = append(conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.DNSReadyIngressConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "NoZones",
			Message: "The record isn't present in any zones.",
		})
	case len(wildcardRecord.Status.Zones) > 0:
		var failedZones []configv1.DNSZone
		for _, zone := range wildcardRecord.Status.Zones {
			for _, cond := range zone.Conditions {
				if cond.Type == iov1.DNSRecordFailedConditionType && cond.Status == string(operatorv1.ConditionTrue) {
					failedZones = append(failedZones, zone.DNSZone)
				}
			}
		}
		if len(failedZones) == 0 {
			conditions = append(conditions, operatorv1.OperatorCondition{
				Type:    operatorv1.DNSReadyIngressConditionType,
				Status:  operatorv1.ConditionTrue,
				Reason:  "NoFailedZones",
				Message: "The record is provisioned in all reported zones.",
			})
		} else {
			// TODO: Add failed condition reasons
			conditions = append(conditions, operatorv1.OperatorCondition{
				Type:    operatorv1.DNSReadyIngressConditionType,
				Status:  operatorv1.ConditionFalse,
				Reason:  "FailedZones",
				Message: fmt.Sprintf("The record failed to provision in some zones: %v", failedZones),
			})
		}
	}

	return conditions
}
