package manifests

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

// The installation's skills reach a machine as a copy of its own, mounted
// read only beside the owner's disk, one folder per skill — never into the
// home, where they would shadow the owner's own and outlive their withdrawal.
func TestMachineSkillsAreMountedReadOnly(t *testing.T) {
	in := machineInput()
	in.Skills = map[string]string{
		"donkey":       "---\nname: donkey\n---\n",
		"donkey-azure": "---\nname: donkey-azure\n---\n",
	}
	objs, err := MachineObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	var clone *corev1.ConfigMap
	for _, obj := range objs {
		if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Labels[LabelRole] == RoleMachineSkills {
			clone = cm
		}
	}
	if clone == nil || clone.Name != MachineSkillsName(in.ObjectName) || clone.Namespace != in.Namespace ||
		clone.Labels[LabelMachine] != in.ID || clone.Data["donkey-azure.md"] != in.Skills["donkey-azure"] || len(clone.Data) != 2 {
		t.Fatalf("skills copy = %+v", clone)
	}

	pod := machineObject[*appsv1.Deployment](t, objs).Spec.Template.Spec
	var volume *corev1.ConfigMapVolumeSource
	for _, v := range pod.Volumes {
		if v.Name == "skills" {
			volume = v.ConfigMap
		}
	}
	if volume == nil || volume.Name != clone.Name || volume.DefaultMode == nil || *volume.DefaultMode != 0o444 || len(volume.Items) != 2 {
		t.Fatalf("skills volume = %+v", volume)
	}
	for i, want := range []string{"donkey", "donkey-azure"} {
		if item := volume.Items[i]; item.Key != want+".md" || item.Path != want+"/SKILL.md" {
			t.Errorf("item %d = %+v, want %s.md as %s/SKILL.md", i, item, want, want)
		}
	}
	var mounts []corev1.VolumeMount
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "skills" {
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 1 || mounts[0].MountPath != config.MachineSkillsPath || !mounts[0].ReadOnly || mounts[0].SubPath != "" {
		t.Fatalf("skills mounts = %+v — one read-only mount of the whole folder", mounts)
	}

	// An installation without skills mounts nothing and copies nothing.
	objs, err = MachineObjects(machineInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Labels[LabelRole] == RoleMachineSkills {
			t.Fatalf("a skills copy without skills: %+v", cm)
		}
	}
	if pod := machineObject[*appsv1.Deployment](t, objs).Spec.Template.Spec; len(pod.Volumes) != 1 || len(pod.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("volumes %+v, mounts %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}
