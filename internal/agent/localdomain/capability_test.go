package localdomain

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func fullCapabilityReport() capabilityReport {
	return capabilityReport{
		UnifiedV2: true, NSDelegate: true, WritableDelegate: true, CgroupKill: true,
		Openat2: true, Statx: true, Clone3Placement: true, CgroupUnshare: true,
		UserPIDMountNS: true, MountCover: true, PidfdOpen: true, PidfdSignal: true,
		MigrationRejected: true, Cleanup: true,
		NestedCgroupMount: nestedMountContained, NestedProcMount: nestedMountContained,
	}
}

func TestCapabilityMatrixFailsEveryRequiredPrimitiveIndependently(t *testing.T) {
	valid := fullCapabilityReport()
	if err := validateCapabilityReport(valid); err != nil {
		t.Fatalf("complete report refused: %v", err)
	}
	typeOf := reflect.TypeOf(valid)
	for i := 0; i < typeOf.NumField(); i++ {
		field := typeOf.Field(i)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			report := valid
			reflect.ValueOf(&report).Elem().Field(i).SetBool(false)
			if err := validateCapabilityReport(report); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("error = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestCapabilityMatrixAcceptsDeniedOrContainedNestedMounts(t *testing.T) {
	for _, outcome := range []nestedMountResult{nestedMountDenied, nestedMountContained} {
		t.Run(strconv.Itoa(int(outcome)), func(t *testing.T) {
			report := fullCapabilityReport()
			report.NestedCgroupMount = outcome
			report.NestedProcMount = outcome
			if err := validateCapabilityReport(report); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, outcome := range []nestedMountResult{nestedMountUnknown, nestedMountExposed} {
		report := fullCapabilityReport()
		report.NestedCgroupMount = outcome
		if err := validateCapabilityReport(report); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("outcome %d error = %v", outcome, err)
		}
	}
}
