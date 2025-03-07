/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package machinehydration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/awstesting/mock"
	"github.com/aws/aws-sdk-go/service/ec2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "knative.dev/pkg/logging/testing"

	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	awscache "github.com/aws/karpenter/pkg/cache"
	"github.com/aws/karpenter/pkg/cloudprovider"
	awscontext "github.com/aws/karpenter/pkg/context"
	"github.com/aws/karpenter/pkg/controllers/machinehydration"
	"github.com/aws/karpenter/pkg/fake"
	"github.com/aws/karpenter/pkg/test"
	"github.com/aws/karpenter/pkg/utils"

	"github.com/aws/karpenter-core/pkg/apis"
	coresettings "github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	corecloudprovider "github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	coretest "github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *coretest.Environment
var unavailableOfferingsCache *awscache.UnavailableOfferings
var ec2API *fake.EC2API
var cloudProvider *cloudprovider.CloudProvider
var hydrationController controller.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine")
}

var _ = BeforeSuite(func() {
	ctx = coresettings.ToContext(ctx, coretest.Settings())
	ctx = settings.ToContext(ctx, test.Settings())
	env = coretest.NewEnvironment(scheme.Scheme, coretest.WithCRDs(apis.CRDs...), coretest.WithFieldIndexers(func(c cache.Cache) error {
		return c.IndexField(ctx, &v1alpha5.Machine{}, "status.providerID", func(o client.Object) []string {
			machine := o.(*v1alpha5.Machine)
			return []string{machine.Status.ProviderID}
		})
	}))
	unavailableOfferingsCache = awscache.NewUnavailableOfferings()
	ec2API = &fake.EC2API{}
	cloudProvider = cloudprovider.New(awscontext.Context{
		Context: corecloudprovider.Context{
			Context:             ctx,
			RESTConfig:          env.Config,
			KubernetesInterface: env.KubernetesInterface,
			KubeClient:          env.Client,
			EventRecorder:       events.NewRecorder(&record.FakeRecorder{}),
			Clock:               &clock.FakeClock{},
			StartAsync:          nil,
		},
		Session:                   mock.Session,
		UnavailableOfferingsCache: unavailableOfferingsCache,
		EC2API:                    ec2API,
	})
	hydrationController = machinehydration.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("MachineHydration", func() {
	var instanceID string
	var providerID string
	BeforeEach(func() {
		ec2API.Reset()
		instanceID = fake.InstanceID()
		providerID = fake.ProviderID(instanceID)

		// Store the instance as existing at DescribeInstances
		ec2API.Instances.Store(
			instanceID,
			&ec2.Instance{
				State: &ec2.InstanceState{
					Name: aws.String(ec2.InstanceStateNameRunning),
				},
				PrivateDnsName: aws.String(fake.PrivateDNSName()),
				InstanceId:     aws.String(instanceID),
			},
		)
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	Context("Successful", func() {
		It("should hydrate from node with basic spec set", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
				Taints: []v1.Taint{
					{
						Key:    "testkey",
						Value:  "testvalue",
						Effect: v1.TaintEffectNoSchedule,
					},
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				Taints: []v1.Taint{
					{
						Key:    "testkey",
						Value:  "testvalue",
						Effect: v1.TaintEffectNoSchedule,
					},
				},
				ProviderID: providerID,
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("1Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("2Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]

			// Expect machine to have populated fields from the node
			Expect(machine.Spec.Taints).To(Equal(provisioner.Spec.Taints))
			Expect(machine.Spec.MachineTemplateRef.APIVersion).To(Equal(provisioner.Spec.ProviderRef.APIVersion))
			Expect(machine.Spec.MachineTemplateRef.Kind).To(Equal(provisioner.Spec.ProviderRef.Kind))
			Expect(machine.Spec.MachineTemplateRef.Name).To(Equal(provisioner.Spec.ProviderRef.Name))

			// Expect that the instance is tagged with the machine-name and cluster-name tags
			instance := ExpectInstanceExists(ec2API, instanceID)
			tag := ExpectMachineTagExists(instance)
			Expect(aws.StringValue(tag.Value)).To(Equal(machine.Name))
			ExpectClusterTagExists(ctx, instance)
		})
		It("should hydrate from node with expected requirements from node labels", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelTopologyZone,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"test-zone-1a", "test-zone-1b", "test-zone-1c"},
					},
					{
						Key:      v1.LabelOSStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{string(v1.Linux), string(v1.Windows)},
					},
					{
						Key:      v1.LabelArchStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{v1alpha5.ArchitectureAmd64},
					},
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
						v1.LabelInstanceTypeStable:       "default-instance-type",
						v1.LabelTopologyRegion:           "coretest-zone-1",
						v1.LabelTopologyZone:             "coretest-zone-1a",
						v1.LabelOSStable:                 string(v1.Linux),
						v1.LabelArchStable:               "amd64",
					},
				},
				ProviderID: providerID,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]

			Expect(machine.Spec.Requirements).To(HaveLen(3))
			Expect(machine.Spec.Requirements).To(ContainElements(
				v1.NodeSelectorRequirement{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-1a", "test-zone-1b", "test-zone-1c"},
				},
				v1.NodeSelectorRequirement{
					Key:      v1.LabelOSStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{string(v1.Linux), string(v1.Windows)},
				},
				v1.NodeSelectorRequirement{
					Key:      v1.LabelArchStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1alpha5.ArchitectureAmd64},
				},
			))

			// Expect that the instance is tagged with the machine-name and cluster-name tags
			instance := ExpectInstanceExists(ec2API, instanceID)
			tag := ExpectMachineTagExists(instance)
			Expect(aws.StringValue(tag.Value)).To(Equal(machine.Name))
			ExpectClusterTagExists(ctx, instance)
		})
		It("should hydrate from node with expected kubelet from provisioner kubelet configuration", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
				Kubelet: &v1alpha5.KubeletConfiguration{
					ClusterDNS:       []string{"10.0.0.1"},
					ContainerRuntime: lo.ToPtr("containerd"),
					MaxPods:          lo.ToPtr[int32](10),
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
				Taints: []v1.Taint{
					{
						Key:    "testkey",
						Value:  "testvalue",
						Effect: v1.TaintEffectNoSchedule,
					},
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("1Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("2Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]

			Expect(machine.Spec.Kubelet).ToNot(BeNil())
			Expect(machine.Spec.Kubelet.ClusterDNS[0]).To(Equal("10.0.0.1"))
			Expect(lo.FromPtr(machine.Spec.Kubelet.ContainerRuntime)).To(Equal("containerd"))
			Expect(lo.FromPtr(machine.Spec.Kubelet.MaxPods)).To(BeNumerically("==", 10))

			// Expect that the instance is tagged with the machine-name and cluster-name tags
			instance := ExpectInstanceExists(ec2API, instanceID)
			tag := ExpectMachineTagExists(instance)
			Expect(aws.StringValue(tag.Value)).To(Equal(machine.Name))
			ExpectClusterTagExists(ctx, instance)
		})
		It("should not hydrate startupTaints into the machine (to ensure they don't get re-applied)", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
				StartupTaints: []v1.Taint{
					{
						Key:    "test-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "test-value",
					},
					{
						Key:    "test-taint2",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "test-value2",
					},
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("1Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("2Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			})

			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]

			// Expect machine to have populated fields from the node
			Expect(machine.Spec.StartupTaints).To(HaveLen(0))

			// Expect that the instance is tagged with the machine-name and cluster-name tags
			instance := ExpectInstanceExists(ec2API, instanceID)
			tag := ExpectMachineTagExists(instance)
			Expect(aws.StringValue(tag.Value)).To(Equal(machine.Name))
			ExpectClusterTagExists(ctx, instance)
		})
		It("should hydrate from node for many machines from many nodes", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
			})
			ExpectApplied(ctx, env.Client, provisioner)

			// Generate 1000 nodes that have different instanceIDs
			var nodes []*v1.Node
			for i := 0; i < 1000; i++ {
				instanceID = fake.InstanceID()
				ec2API.EC2Behavior.Instances.Store(
					instanceID,
					&ec2.Instance{
						State: &ec2.InstanceState{
							Name: aws.String(ec2.InstanceStateNameRunning),
						},
						PrivateDnsName: aws.String(fake.PrivateDNSName()),
						InstanceId:     aws.String(instanceID),
					},
				)
				node := coretest.Node(coretest.NodeOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
							v1alpha5.LabelNodeInitialized:    "true",
						},
					},
					ProviderID: fake.ProviderID(instanceID),
					Allocatable: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1"),
					},
					Capacity: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2"),
					},
				})
				nodes = append(nodes, node)
			}

			// Generate a reconcile loop for all the nodes simultaneously
			workqueue.ParallelizeUntil(ctx, 50, len(nodes), func(i int) {
				ExpectApplied(ctx, env.Client, nodes[i])
				ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(nodes[i]))
			})
			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1000))

			for _, node := range nodes {
				instance := ExpectInstanceExists(ec2API, lo.Must(utils.ParseInstanceID(node.Spec.ProviderID)))
				ExpectMachineTagExists(instance)
				ExpectClusterTagExists(ctx, instance)
			}
		})
		It("should hydrate from node by pulling the hydrated machine's name from the HydrateMachine cloudProvider call", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)

			expectedName := "my-custom-machine"

			// Set the DescribeInstancesOutput to return an instance with a MachineName label
			ec2API.DescribeInstancesBehavior.Output.Set(&ec2.DescribeInstancesOutput{
				Reservations: []*ec2.Reservation{
					{
						Instances: []*ec2.Instance{
							{
								Tags: []*ec2.Tag{
									{
										Key:   aws.String(v1alpha5.MachineNameLabelKey),
										Value: aws.String(expectedName),
									},
								},
								State: &ec2.InstanceState{
									Name: aws.String(ec2.InstanceStateNameRunning),
								},
								InstanceId:     aws.String(instanceID),
								PrivateDnsName: aws.String(fake.PrivateDNSName()),
							},
						},
					},
				},
			})

			// Expect that we go to hydrate machines, and we don't add extra machines for the existing one
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))
			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]

			// Expect that we hydrated the machine based on the cloudProvider response
			Expect(machine.Name).To(Equal(expectedName))

			// Expect that we added the cluster tag to the instance
			instance := ExpectInstanceExists(ec2API, instanceID)
			ExpectClusterTagExists(ctx, instance)
		})
		It("should hydrate from node using provider and no providerRef", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				Provider: v1alpha1.AWS{
					AMIFamily:             aws.String(v1alpha1.AMIFamilyAL2),
					SubnetSelector:        map[string]string{"*": "*"},
					SecurityGroupSelector: map[string]string{"*": "*"},
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
				Taints: []v1.Taint{
					{
						Key:    "testkey",
						Value:  "testvalue",
						Effect: v1.TaintEffectNoSchedule,
					},
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("1Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("2Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
			machine := machineList.Items[0]
			Expect(machine.Annotations).To(HaveKey(v1alpha5.ProviderCompatabilityAnnotationKey))

			// Expect that the instance is tagged with the machine-name and cluster-name tags
			instance := ExpectInstanceExists(ec2API, instanceID)
			tag := ExpectMachineTagExists(instance)
			Expect(aws.StringValue(tag.Value)).To(Equal(machine.Name))
			ExpectClusterTagExists(ctx, instance)
		})
	})
	Context("Failed", func() {
		It("should not hydrate from node without a provisioner label", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.LabelNodeInitialized: "true",
					},
				},
				ProviderID: providerID,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(0))
		})
		It("should not hydrate from node without a provisioner that exists", func() {
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: "default",
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(0))
		})
		It("should not hydrate from node for a node that is already hydrated", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
			})
			m := coretest.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: node.Spec.ProviderID, // Same providerID as the node
				},
			})
			ExpectApplied(ctx, env.Client, provisioner, node, m)

			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))

			// Expect that we go to hydrate machines, and we don't add extra machines for the existing one
			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(1))
		})
		It("should not hydrate from node for an instance that is terminated", func() {
			provisioner := coretest.Provisioner(coretest.ProvisionerOptions{
				ProviderRef: &v1alpha5.ProviderRef{
					APIVersion: v1alpha5.TestingGroup + "v1alpha1",
					Kind:       "NodeTemplate",
					Name:       "default",
				},
			})
			node := coretest.Node(coretest.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				ProviderID: providerID,
			})
			ExpectApplied(ctx, env.Client, provisioner, node)

			// Update the state of the existing instance
			instance := ExpectInstanceExists(ec2API, instanceID)
			instance.State.Name = aws.String(ec2.InstanceStateNameTerminated)
			ec2API.Instances.Store(instanceID, instance)

			ExpectReconcileSucceeded(ctx, hydrationController, client.ObjectKeyFromObject(node))
			machineList := &v1alpha5.MachineList{}
			Expect(env.Client.List(ctx, machineList)).To(Succeed())
			Expect(machineList.Items).To(HaveLen(0))
		})
	})
})

func ExpectInstanceExists(api *fake.EC2API, instanceID string) *ec2.Instance {
	raw, ok := api.Instances.Load(instanceID)
	Expect(ok).To(BeTrue())
	return raw.(*ec2.Instance)
}

func ExpectMachineTagExists(instance *ec2.Instance) *ec2.Tag {
	tag, ok := lo.Find(instance.Tags, func(t *ec2.Tag) bool {
		return aws.StringValue(t.Key) == v1alpha5.MachineNameLabelKey
	})
	Expect(ok).To(BeTrue())
	return tag
}

func ExpectClusterTagExists(ctx context.Context, instance *ec2.Instance) *ec2.Tag {
	tag, ok := lo.Find(instance.Tags, func(t *ec2.Tag) bool {
		return aws.StringValue(t.Key) == fmt.Sprintf("kubernetes.io/cluster/%s", settings.FromContext(ctx).ClusterName)
	})
	Expect(ok).To(BeTrue())
	return tag
}
