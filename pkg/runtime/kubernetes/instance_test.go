package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func nsWith(name string, managed bool, instance string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		ns.Annotations = map[string]string{manifests.AnnotationManaged: "true"}
	}
	if instance != "" {
		ns.Labels = map[string]string{manifests.LabelInstance: instance}
	}
	return ns
}

// Two installations with the same prefix map a space onto the same namespace.
// "Free" must mean "mine or nobody's" — a namespace another donkey manages is
// the one thing worse to adopt than a stranger's, because adopting rewrites
// its labels and makes it deletable from here.
func TestCheckNamespaceFreeAcrossInstallations(t *testing.T) {
	cases := []struct {
		name     string
		ns       *corev1.Namespace // nil: doesn't exist
		check    string            // the namespace the space maps onto
		instance string            // "" is the default installation
		wantFree bool
	}{
		{"missing is free", nil, "donkey-x", "", true},
		{"own space namespace", nsWith("donkey-x", true, "donkey-system"), "donkey-x", "", true},
		// Everything donkey applies is stamped, so an unstamped namespace was
		// written by something else — donkey-shaped or not, it isn't ours.
		{"unstamped managed namespace", nsWith("donkey-x", true, ""), "donkey-x", "", false},
		{"another installation's namespace", nsWith("donkey-x", true, "bank-donkey-system"), "donkey-x", "", false},
		{"somebody else's namespace entirely", nsWith("donkey-x", false, ""), "donkey-x", "", false},
		{"the installation's own home", nsWith("donkey-system", true, "donkey-system"), "donkey-system", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			if tc.ns != nil {
				if _, err := cs.CoreV1().Namespaces().Create(context.Background(), tc.ns, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			err := (&Client{Clientset: cs}).ForInstance(tc.instance).CheckNamespaceFree(context.Background(), tc.check)
			if free := err == nil; free != tc.wantFree {
				t.Fatalf("free = %v (%v), want %v", free, err, tc.wantFree)
			}
		})
	}
}

// Deleting follows the same line: a namespace another installation manages is
// not this one's to delete, however donkey-shaped it looks.
func TestDeleteSpaceRefusesForeignNamespace(t *testing.T) {
	cs := fake.NewClientset()
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(),
		nsWith("donkey-x", true, "bank-donkey-system"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c := (&Client{Clientset: cs}).ForInstance("")
	if err := c.DeleteSpace(context.Background(), "donkey-x"); err == nil {
		t.Fatal("another installation's namespace was deleted")
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "donkey-x", metav1.GetOptions{}); err != nil {
		t.Fatalf("the namespace should still be there: %v", err)
	}
}
