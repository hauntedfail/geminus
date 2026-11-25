/- Verification policy, not a Git or cryptography implementation. The caller
   supplies an independently pinned ID, authoritative requirements, a resolver,
   a signer-policy verifier for the requested commit, and authenticated evidence.
   None of those external facts can be established by this pure model. -/
namespace Completion

abbrev Check (α : Type) := Except String α

structure Evidence where
  requirement : String
  revision : String
  trusted : Bool
  passed : Bool

def fullId (s : String) : Bool :=
  (s.length == 40 || s.length == 64) && s.toList.all fun c =>
    ('0' ≤ c && c ≤ '9') || ('a' ≤ c && c ≤ 'f')

def covered (rows : List Evidence) (id req : String) : Bool :=
  rows.any fun e => e.requirement == req && e.revision == id && e.trusted && e.passed

def gate (expected : String) (resolved : Check String) (signature : String → Check Bool)
    (requirements : List String) (evidence : Check (List Evidence)) : Bool :=
  match resolved, evidence with
  | .ok id, .ok rows =>
    fullId expected && (id == expected) && !requirements.isEmpty &&
      (match signature id with | .ok true => true | _ => false) &&
      requirements.all (covered rows expected)
  | _, _ => false

/-- Even a valid signature cannot compensate for a missing qualifying row. -/
theorem no_missing {expected req : String} {resolved : Check String}
    {signature : String → Check Bool} {requirements : List String} {rows : List Evidence}
    (required : req ∈ requirements) (missing : covered rows expected req = false) :
    gate expected resolved signature requirements (.ok rows) = false := by
  have incomplete : requirements.all (covered rows expected) = false := by
    cases h : requirements.all (covered rows expected) with
    | false => rfl
    | true =>
      have present := List.all_eq_true.mp h req required
      simp [missing] at present
  cases resolved <;> simp [gate, incomplete]

#print axioms no_missing

section Fixtures
private def pin := String.mk (List.replicate 40 'a')
private def other := String.mk (List.replicate 40 'b')
private def sig (id : String) : Check Bool := .ok (id == pin)
private def build : Evidence := ⟨"build", pin, true, true⟩
private def testRow : Evidence := ⟨"test", pin, true, true⟩
private def run := gate pin (.ok pin) sig ["build", "test"]

#guard run (.ok [testRow, build])
#guard !run (.ok [build, build])
#guard !run (.ok [build, {testRow with revision := other}])
#guard !run (.ok [build, {testRow with trusted := false}])
#guard !run (.ok [build, {testRow with passed := false}])
#guard !run (.error "evidence unavailable")
#guard !gate pin (.ok other) (fun _ => .ok true) ["build"] (.ok [{build with revision := other}])
#guard !gate pin (.ok pin) sig [] (.ok [])
#guard !gate pin (.ok pin) (fun _ => .ok false) ["build"] (.ok [build])
#guard !gate pin (.ok pin) (fun _ => .error "signature unavailable") ["build"] (.ok [build])
#guard !gate pin (.error "unresolved") sig ["build"] (.ok [build])
#guard !gate "abc" (.ok "abc") (fun _ => .ok true) ["build"] (.ok [{build with revision := "abc"}])
#guard !fullId (String.mk (List.replicate 40 'g'))
#guard fullId (String.mk (List.replicate 64 '0'))
end Fixtures
end Completion
