package reconcilertesting

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	v2 "github.com/kyma-project/kyma/components/eventing-controller/testing/v2"
	"github.com/stretchr/testify/require"

	"github.com/avast/retry-go/v3"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	eventingv1alpha2 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha2"
	"github.com/kyma-project/kyma/components/eventing-controller/controllers/events"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/env"
)

const (
	SmallTimeout           = 20 * time.Second
	SmallPollingInterval   = 1 * time.Second
	subscriptionNameFormat = "nats-sub-%d"
	retryAttempts          = 5
	MaxReconnects          = 10
)

type TestEnsemble struct {
	testID                    int
	Cfg                       *rest.Config
	K8sClient                 client.Client
	TestEnv                   *envtest.Environment
	NatsServer                *natsserver.Server
	NatsPort                  int
	DefaultSubscriptionConfig env.DefaultSubscriptionConfig
	SubscriberSvc             *v1.Service
	Cancel                    context.CancelFunc
	Ctx                       context.Context
	G                         *gomega.GomegaWithT
	T                         *testing.T
}

type Want struct {
	K8sSubscription []gomegatypes.GomegaMatcher
	K8sEvents       []v1.Event
	// NatsSubscriptions holds gomega matchers for a NATS subscription per event-type.
	NatsSubscriptions map[string][]gomegatypes.GomegaMatcher
}

// EventuallyUpdateSubscriptionOnK8s updates a given sub on kubernetes side.
// In order to be resilient and avoid a conflict, the update operation is retried until the update succeeds.
// To avoid a 409 conflict, the subscription CR data is read from the apiserver before a new update is performed.
// This conflict can happen if another entity such as the eventing-controller changed the sub in the meantime.
func EventuallyUpdateSubscriptionOnK8s(ctx context.Context, ens *TestEnsemble,
	sub *eventingv1alpha2.Subscription, updateFunc func(*eventingv1alpha2.Subscription) error) {
	ens.G.Eventually(func() error {
		// get a fresh version of the Subscription
		lookupKey := types.NamespacedName{
			Namespace: sub.Namespace,
			Name:      sub.Name,
		}
		if err := ens.K8sClient.Get(ctx, lookupKey, sub); err != nil {
			return errors.Wrapf(err, "error while fetching subscription %s", lookupKey.String())
		}
		if err := updateFunc(sub); err != nil {
			return err
		}
		return nil
	}, SmallTimeout, SmallPollingInterval).ShouldNot(gomega.HaveOccurred(), "error while updating the subscription")
}

func CreateSubscription(ens *TestEnsemble, subscriptionOpts ...v2.SubscriptionOpt) *eventingv1alpha2.Subscription {
	subscriptionName := fmt.Sprintf(subscriptionNameFormat, ens.testID)
	ens.testID++
	subscription := v2.NewSubscription(subscriptionName, ens.SubscriberSvc.Namespace, subscriptionOpts...)
	subscription = createSubscriptionInK8s(ens, subscription)
	return subscription
}

func TestSubscriptionOnK8s(ens *TestEnsemble, subscription *eventingv1alpha2.Subscription,
	expectations ...gomegatypes.GomegaMatcher) {
	description := "Failed to match the eventing subscription"
	expectations = append(expectations, v2.HaveSubscriptionName(subscription.Name))
	getSubscriptionOnK8S(ens, subscription).Should(gomega.And(expectations...), description)
}

func TestEventsOnK8s(ens *TestEnsemble, expectations ...v1.Event) {
	for _, event := range expectations {
		getK8sEvents(ens).Should(v2.HaveEvent(event), "Failed to match k8s events")
	}
}

func ValidSinkURL(ens *TestEnsemble, additions ...string) string {
	url := v2.ValidSinkURL(ens.SubscriberSvc.Namespace, ens.SubscriberSvc.Name)
	for _, a := range additions {
		url = fmt.Sprintf("%s%s", url, a)
	}
	return url
}

// IsSubscriptionDeletedOnK8s checks a subscription is deleted and allows making assertions on it.
func IsSubscriptionDeletedOnK8s(ens *TestEnsemble, subscription *eventingv1alpha2.Subscription) gomega.AsyncAssertion {
	g := ens.G

	return g.Eventually(func() bool {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		if err := ens.K8sClient.Get(ens.Ctx, lookupKey, subscription); err != nil {
			return k8serrors.IsNotFound(err)
		}
		return false
	}, SmallTimeout, SmallPollingInterval)
}

func ConditionInvalidSink(msg string) eventingv1alpha2.Condition {
	return eventingv1alpha2.MakeCondition(
		eventingv1alpha2.ConditionSubscriptionActive,
		eventingv1alpha2.ConditionReasonNATSSubscriptionNotActive,
		v1.ConditionFalse, msg)
}

func EventInvalidSink(msg string) v1.Event {
	return v1.Event{
		Reason:  string(events.ReasonValidationFailed),
		Message: msg,
		Type:    v1.EventTypeWarning,
	}
}

func StartTestEnv(ens *TestEnsemble) {
	var err error
	var k8sCfg *rest.Config

	err = retry.Do(func() error {
		defer func() {
			if r := recover(); r != nil {
				log.Println("panic recovered:", r)
			}
		}()

		k8sCfg, err = ens.TestEnv.Start()
		return err
	},
		retry.Delay(time.Minute),
		retry.DelayType(retry.FixedDelay),
		retry.Attempts(retryAttempts),
		retry.OnRetry(func(n uint, err error) {
			log.Printf("[%v] try failed to start testenv: %s", n, err)
			if stopErr := ens.TestEnv.Stop(); stopErr != nil {
				log.Printf("failed to stop testenv: %s", stopErr)
			}
		}),
	)

	require.NoError(ens.T, err)
	require.NotNil(ens.T, k8sCfg)
	ens.Cfg = k8sCfg
}

func StopTestEnv(ens *TestEnsemble) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("panic recovered:", r)
		}
	}()

	if stopErr := ens.TestEnv.Stop(); stopErr != nil {
		log.Printf("failed to stop testenv: %s", stopErr)
	}
}

func StartSubscriberSvc(ens *TestEnsemble) {
	ens.SubscriberSvc = v2.NewSubscriberSvc("test-subscriber", "test")
	createSubscriberSvcInK8s(ens)
}

// createSubscriberSvcInK8s ensures the subscriber service in the k8s cluster is created successfully.
// The subscriber service is taken from the TestEnsemble struct and should not be nil.
// If the namespace of the subscriber service does not exist, it will be created.
func createSubscriberSvcInK8s(ens *TestEnsemble) {
	g := ens.G

	// if the namespace is not "default" create it on the cluster
	if ens.SubscriberSvc.Namespace != "default " {
		namespace := fixtureNamespace(ens.SubscriberSvc.Namespace)
		if namespace.Name != "default" {
			g.Eventually(func() error {
				if err := ens.K8sClient.Create(ens.Ctx, namespace); !k8serrors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, SmallTimeout, SmallPollingInterval).
				ShouldNot(gomega.HaveOccurred(), "Failed to to create the namespace for the subscriber")
		}
	}

	g.Eventually(func() error {
		// create subscriber svc on cluster
		return ens.K8sClient.Create(ens.Ctx, ens.SubscriberSvc)
	}, SmallTimeout, SmallPollingInterval).ShouldNot(gomega.HaveOccurred(), "Failed to create the subscriber service")
}

// createSubscriptionInK8s creates a Subscription on the K8s client of the testEnsemble. All the reconciliation
// happening will be reflected in the subscription.
func createSubscriptionInK8s(ens *TestEnsemble,
	subscription *eventingv1alpha2.Subscription) *eventingv1alpha2.Subscription {
	g := ens.G

	if subscription.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(subscription.Namespace)
		if namespace.Name != "default" {
			err := ens.K8sClient.Create(ens.Ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				require.NoError(ens.T, err)
			}
		}
	}

	// create subscription on cluster
	g.Eventually(func() error {
		return ens.K8sClient.Create(ens.Ctx, subscription)
	}, SmallTimeout, SmallPollingInterval).ShouldNot(gomega.HaveOccurred(), "Failed to create subscription")
	return subscription
}

func fixtureNamespace(name string) *v1.Namespace {
	namespace := v1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "natsNamespace",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return &namespace
}

// getSubscriptionOnK8S fetches a subscription using the lookupKey and allows making assertions on it.
func getSubscriptionOnK8S(ens *TestEnsemble, subscription *eventingv1alpha2.Subscription) gomega.AsyncAssertion {
	g := ens.G

	return g.Eventually(func() *eventingv1alpha2.Subscription {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		if err := ens.K8sClient.Get(ens.Ctx, lookupKey, subscription); err != nil {
			return &eventingv1alpha2.Subscription{}
		}
		return subscription
	}, SmallTimeout, SmallPollingInterval)
}

// getK8sEvents returns all kubernetes events for the given namespace.
// The result can be used in a gomega assertion.
func getK8sEvents(ens *TestEnsemble) gomega.AsyncAssertion {
	g := ens.G

	eventList := v1.EventList{}
	return g.Eventually(func() v1.EventList {
		err := ens.K8sClient.List(ens.Ctx, &eventList, client.InNamespace(ens.SubscriberSvc.Namespace))
		if err != nil {
			return v1.EventList{}
		}
		return eventList
	}, SmallTimeout, SmallPollingInterval)
}

func NewUncleanEventType(ending string) string {
	return fmt.Sprintf("%s%s", v2.OrderCreatedEventTypeNotClean, ending)
}

func NewCleanEventType(ending string) string {
	return fmt.Sprintf("%s%s", v2.OrderCreatedEventType, ending)
}
