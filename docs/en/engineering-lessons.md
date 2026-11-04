# Engineering lessons — criteria shape

> **Status: translation of the criteria-shape sections.** Source of truth:
> [`docs/zh/engineering-lessons.md`](../zh/engineering-lessons.md).
> Sections §1 (criteria that lie) and §5 (criteria shape) are translated in full;
> §2–§4 are summarized at the end. If the two disagree, the Chinese version wins —
> and this file should be corrected.

Why this document is worth the effort: the failure shapes below recur. They are
more durable than any particular implementation, and each one cost a production
incident to learn.

---

## §1 Criteria that lie

### 1.1 Structural assertions are not quality assertions

**Symptom**: 16 criteria, 6 mutations, **all green** — and the feature was completely
unusable.

**The case**: a memory-retrieval rewrite. The criteria checked "field is non-empty",
"row count is right", "does not panic". Actual retrieval results:

```text
query "how is doc-layer recall designed"  → score=0.7834
query "hmap plugin install verification"  → score=0.7922
query "QQ avatar"                         → score=0.7601
```

Every score landed in 0.75–0.79. **Discrimination ≈ 0**; the returned content had
nothing to do with the query.

⇒ "All green" proved only that *I implemented what I thought I implemented*.

**Ask**: would this criterion go green if the feature were completely dead? If yes,
it is not a quality criterion.

### 1.2 Testing "after wiring" ≠ testing "is it wired"

The most frequent defect shape in this repo (5 instances so far).

| Criterion | What it does | Why it is always green |
| --- | --- | --- |
| `security_toolgate_test.go` | calls `a.SetSecurity(rt)` manually, then asserts interception | **the criterion supplies the missing wiring itself** |
| `retention_test.go` | writes `month_2026_01.tar.gz` by hand, then asserts deletion | never ran `compressMonthly`'s **real output** (which produces `month_2026-27` — parsing necessarily fails) |

Both green. Both features dead in production.

**Ask**: would this criterion still pass under the *production* assembly?

### 1.3 "Verification passed" ≠ "architecture is right"

16 end-to-end triple-extraction checks passed. But they verified *extraction quality*
(can a sentence be split into subject-predicate-object), not *the correct storage
shape*. The whole approach was rejected after the verification.

**Ask**: am I verifying the thing I actually want to guarantee?

### 1.4 Fixtures invented from imagined fields

The fixture used hand-written field names that did not match the real struct, so the
criterion tested a contract that never existed.

⇒ Criterion inputs must come from **real artifacts**, not from memory.

### 1.5 Writing tests after changing the code

Changed the code first, added tests second. The tests then only proved "I changed
what I thought I changed".

---

## §5 Criteria shape

§1–§4 describe how criteria *lie*. §5 describes why a criterion's **shape** can be
wrong — which is more expensive, because a mis-shaped criterion freezes an
implementation detail into a contract. Refactoring the internals then necessarily
breaks the criterion, and the author must choose: don't change the code, or delete
the criterion.

### 5.1 A criterion must treat the module as a black box

**The point of modularization is that internal changes do not break external
interfaces.** A criterion that does not respect that layer cancels the benefit.

White-box criteria turn internal state into contract. Measured costs:

| White-box criterion | What it froze | Consequence |
| --- | --- | --- |
| `if sf.Step != StepLLM` | which state-machine step it stops at | renaming or splitting `StepLLM` turns it red while behavior is correct |
| `if f.ToolIdx == 3` | how the loop advances | switching to batch-commit turns it red; the result is identical |
| `strings.Contains(src, "kstate.Mount(ks)")` | a source literal | rewriting as `kstate.Mount(state)` turns it red (that rewrite is correct) |
| `os.ReadFile("../../events/bus.go")` + regex | which file a constant lives in | moving the constant to `types.go` turns it red |

**Ask**: does this criterion constrain an *external promise*, or *how it happens to be
implemented today*? If the latter, it should not exist as a criterion — it belongs in
a code comment explaining why the code is shaped that way.

#### Three legitimate exceptions

1. **Pure algorithms** — input/output *is* the whole semantics (trimming, version
   comparison, pagination). White-box here does not freeze "flow".
2. **Concurrency / lifetime invariants** — timing constraints invisible from outside,
   e.g. "emit must not read a released pointer during Dispose". These must be pinned
   with `-race` plus concurrent pressure, **not** by asserting internal fields.
3. **Circuit-breaker / retry state machines** — the internal state is itself required
   by the spec ("3 crashes ⇒ open"). Assert the **observable effect** of the
   transition (is it still healthy? did it enter the reload queue?), not the field value.

#### Operable test

> **"If I rewrote this module's internals completely, keeping the public interface
> identical, would this criterion still be green?"**
>
> Green ⇒ shape is right. Red ⇒ it pins the implementation, not the contract.

### 5.2 Do not assert on source text

`os.ReadFile("*.go")` + `strings.Contains` / `regexp` is an **anti-pattern**, even when
the motive is sound (e.g. "prevent someone from deleting the wiring line").

**Why it looks attractive**: reading source is cheap compared to building a heavy
fixture.

**Why it must be banned**:

- It tests **text**, not **semantics**. Adding a space, renaming a variable, or an
  equivalent refactor breaks the check (false red or false green).
- It makes file layout part of the contract. Moving code to a new file turns it red.
- It cannot distinguish "the code exists" from "the code runs".

**Correct alternatives**, in priority order:

| What you want to guarantee | Wrong | Right |
| --- | --- | --- |
| production assembly wires something | grep for `kstate.Mount(ks)` | start the real assembly from zero, assert an **observable result** (`kstate.Get() != nil`) |
| both protocol ends agree on fields | regex-extract string literals on both sides | start the handler, send a real event, assert the **received JSON** |
| a guard was not deleted | grep the source | construct the trigger condition, assert the **guard fires** (request rejected) |
| nobody deletes a criterion | grep the test file for a function name | CI-level guard: `git diff --name-status` detects the deletion |

**Only exception**: CI configuration self-checks (e.g. "the exclusion table must not
silently degrade"). There the *object under test is the config file itself* and it has
no other observable behavior. Even then, the comment must say so explicitly.

### 5.2.1 A criterion must not cite documentation

**"Tests citing docs" is wrong on its face** — docs can be edited, deleted, rewritten,
while criteria must be anchored to **current code behavior**.

Real counter-example (`internal/agent/core/interrupt_invariant_test.go:336`):

```go
// Fact list (from probing, see docs/zh/interrupt-subsystem-split-plan.md §1.5).
// This only asserts "these fields exist on scheduler", as an anchor for the rewrite;
```

Three layers of wrong:

1. **The cited document is an unimplemented plan** — it is itself labeled
   "status: plan (not implemented)". The criterion is anchored to a fact that does
   not exist.
2. **It tests no contract.** The actual assertion is "after enqueueing, the queue has
   1 item" — the most basic correctness of interrupt handling, unrelated to the
   claimed "reverse-dependency snapshot".
3. **It turns undone work into a passing criterion.** Nothing can turn it red except
   a total failure of the enqueue function — at which point other criteria already fail.

⇒ The right move is to **delete it** (or rewrite it into a real criterion) and record
the TODO in `future-plan.md`. **There is no legitimate shape called "a permanently
green criterion holding a TODO".**

**Ask**: is this criterion's basis code behavior, or a document that may change? If
the latter, it protects prose, not the system.

### 5.3 Separate test files from business code

**Measured state (2026-11-04)**:

```text
test files repo-wide        521
test lines of code          112,068
internal/agent/core/        196 files, 144 of them tests (73%)
internal/plugins/webui/     29 test files, all `package webui` (white-box)
repo-wide `package X_test`  only 3
```

The problem is not "a lot of tests". It is:

1. `ls internal/agent/core/` does not show what the module consists of.
2. 100% white-box ⇒ 100% of criteria can touch internals.
3. Converting to black-box requires moving files — and the source-scanning criteria
   block the move.

**Convention** (mandatory for new code, migrate existing incrementally):

- Black-box criteria use `package <pkg>_test`, in `*_contract_test.go`.
- White-box criteria (only the three exceptions in §5.1) go in `*_internal_test.go`
  with a header explaining *why white-box is unavoidable*.
- New criteria default to black-box. Writing white-box requires answering in the file
  header: can this be expressed black-box? Why not?

**This is not a style preference**: if 90% of a package's criteria are white-box, that
package has no module boundary in practice — nobody dares change its internals.

### 5.3.1 Three real limits on test placement (measured)

After writing §5.3 I acted on it and hit three cases that cannot be migrated. Recording
them so nobody (including me) assumes "all tests can move to test/".

#### Works: a `<pkg>/test/` subdirectory

Go allows `pkg` and `pkg_test` in one directory, and allows `<pkg>/test/` as a separate
package. **Key fact**: writing `package pkg` inside `pkg/test/` still cannot reach
unexported symbols —

```go
// pkg/test/a_test.go
package pkg
func TestX(t *testing.T) { _ = internalVar }   // undefined: internalVar
```

Go partitions packages **by directory**. So a subdirectory and an external package are
equivalent in terms of visibility: both force black-box.

#### Cannot migrate 1: `package main` (CLI)

```text
import "x/cmd" is a program, not an importable package
```

`tools/hmapdev` (19 test files), `cmd/homed`, etc. **must stay in-package**.

#### Cannot migrate 2: tests that construct private fields

SDK tests wrote `&PluginSDK{regStage: regStage, name: "test"}` — after migration they
cannot even **construct the object under test**
(`cannot refer to unexported field regStage`).

**But this does not mean white-box is required**: `New(name, sett, regTool, regStage,
regAPI, regOutput)` already takes these as parameters. It is equivalent *and* it is the
production path. All 19 instances were convertible, and the criteria still caught
mutations afterwards.

⇒ When you "need to set a private field", first ask: **is there an exported constructor
that already accepts it?** Most of the time there is; the test was just taking a shortcut.

#### Cannot migrate 3: criteria that scan "the current directory's source"

```go
files, _ := filepath.Glob("*.go")     // childkind_test.go
```

After moving to a subdirectory this globs an **empty directory**, and the criterion
silently loses its subject — no compile error, just green. Convert these to a fixed
relative path, or rewrite per §5.2.

#### Migration order: one file at a time, let the compiler talk

My first attempt migrated 11 files in bulk and then ran a script to prepend `core.`
to identifiers. The script also rewrote `import "…/internal/agent/core"` itself
(producing `core.core.core`) and clobbered `strconv.Itoa`.

Text substitution is unreliable for "prepend a package qualifier" — it hits import
paths, comments, and standard-library symbols. **Migrate one file, read the compiler
error, fix by hand.** Slow, but it leaves no half-migrated state (`agent/core` had to
be fully rolled back once because of this).

### 5.3.2 Coverage requires `-coverpkg=./...`

After moving criteria to `<pkg>/test/`, the **default** coverage accounting loses them:

```bash
go test ./... -coverprofile=cover.out
```

It only counts coverage measured *from tests inside each package*. But the tests
covering `child` live in `child/test/` — a different package. So the main report shows:

```text
internal/agent/core/child        0.0%     ← wrong
internal/agent/core/child/test   87.6%    ← actually covering child
```

With `-coverpkg=./...`:

```text
internal/agent/core/child       85.7%
global                           56.9% → 63.0%
```

**The earlier number was a systematic underestimate.** Under-reporting is more dangerous
than over-reporting: it pushes people to "write empty criteria to raise the number",
which is exactly the trap in §1.1.

⇒ Separating criteria (§5.3) and collecting coverage **must change together**. Change
only the former and the metric degrades into a lie.

### 5.4 CI must cover every independent module

**Measured gap (2026-11-04)**: the repo contains `third_party/hologramagentsdk/`, which
has its own `go.mod` **and its own `.git`** (remote:
`github.com/JianFeeeee/hologramagentsdk`).

Consequence:

```text
go list ./... | grep third_party   →  0
```

The main repo's `go test ./...` **never includes** any SDK test.

- The SDK repo's own CI does run them (its `.github/workflows/ci.yml` has
  `go test ./...` and separately handles the nested module)
- But the **main repo's CI only verifies "it compiles"**, not "the SDK's criteria
  still pass"

⇒ A main-repo change may depend on SDK internals, and the main repo will not notice
when SDK criteria go red.

**Rules**:

1. A `go.mod` in a subdirectory ⇒ it is an independent module, invisible to `go test ./...`
2. Every independent module needs its **own** test step in CI
3. A directory with its own `.git` is **not managed by the main repo**; the main repo
   only vendors what it needs. When `go.mod` `replace`s it, CI must ensure those files
   exist and match upstream HEAD

### 5.5 CI and local must run the same gates

**Measured gap**: the commit discipline requires `gofmt -l` to be clean, but `gofmt`
appeared **0 times** across three workflows. The rule was enforced only by human
diligence. Same for `staticcheck` / `govulncheck` / coverage.

**Rule**: every item written into the commit discipline must have a corresponding CI
step. Otherwise it is a **suggestion**, not a discipline.

| Discipline item | CI backstop |
| --- | --- |
| `go build ./...` | ✅ |
| `go vet ./...` | ✅ |
| `gofmt -l` clean | ✅ (added 2026-11-04) |
| `go test -race` green | ✅ |
| clean worktree compiles | ❌ human only |

---

## §2–§4 (summarized)

**§2 Wiring.** Mechanism existing ≠ being wired (`codegraph_callers` returning 0 is
the test). The dependency graph is the only thing that guarantees init order — reading
another module's field without declaring the facility dependency works only by layer
coincidence. When you change the singular path, do not forget the plural one.

**§3 Permissions & lifetime.** "Wired up" has three levels: mechanism exists → called
from the execution chain → **model is correct**. Tables keyed by a caller-supplied
name must be cleaned up when the lifetime ends, or same-name re-creation inherits the
previous generation's state. Privilege escalation must be derived by the security
module from *facts*, never declared by the caller.

**§4 Process.** Never run memory-heavy exports concurrently with tests. Numeric claims
in docs must be produced by a command, not recalled from memory. Plans must not enter
the repository — after completion they read as "what we intended" while being read as
"what is".

---

## Appendix: the common shape

> **There is always a gap between a signal that looks right and actually being right.**

- criteria all green ≠ feature usable
- mechanism exists ≠ wired
- layer index happens to be right ≠ dependency declared correctly
- singular path right ≠ plural path right
- same name ≠ same thing
- plan written ≠ work done
- **criterion passes ≠ contract protected** (it may pin the implementation, §5.1)
- **`go test ./...` green ≠ all tests ran** (independent modules are excluded, §5.4)
- **discipline written down ≠ anyone enforces it** (without a CI backstop it is advice, §5.5)

⇒ When writing a criterion, a document, or code, ask: **under what circumstances would
this signal be false?**
