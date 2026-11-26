package chat

import (
	"github.com/hauntedfail/geminus/internal/wire"
)

// priorDirectories recalls only owner-signed conversations with a matching
// owner-signed prompt and a successful, final bridge-signed output for it.
// A queued prompt, failed run, forged result, or another persona is not evidence.
func (a *App) priorDirectories() ([]string, error) {
	sessions, _, err := a.conversations()
	if err != nil {
		return nil, err
	}
	var dirs []string
	seen := map[string]bool{}
	for _, s := range sessions {
		info, _ := sessionInfo(s)
		if info.CWD == "" || seen[info.CWD] || validDir(info.CWD) != nil {
			continue
		}
		ctx, cancel := a.call()
		messages, err := a.Honcho.Messages(ctx, s.ID)
		cancel()
		if err != nil {
			return nil, err
		}
		prompts := map[string]bool{}
		for _, m := range messages {
			meta, ok := parseMeta(m)
			if !ok || meta.Role != wire.RolePrompt || m.PeerID != a.Profile.Human {
				continue
			}
			label := wire.PromptLabel(s.ID, info.Persona, info.Harness, info.Human, meta.Nonce, info.CWD,
				wire.Tuning{Model: meta.Model, Effort: meta.Effort, Mode: meta.Mode})
			if a.humanAuth().Verify(m.Content, label, meta.TS, meta.Sig, meta.SigAlg) {
				prompts[m.ID] = true
			}
		}
		for _, m := range messages {
			meta, ok := parseMeta(m)
			if !ok || meta.Role != wire.RoleOutput || !meta.OK || !meta.Final || !prompts[meta.Turn] || m.PeerID != info.Persona {
				continue
			}
			label := wire.OutputLabel(s.ID, meta.Turn, meta.Seq, meta.Final, meta.OK, meta.Native)
			if wire.Verify(a.Profile.Secret, m.Content, label, meta.TS, meta.Sig) {
				dirs = append(dirs, info.CWD)
				seen[info.CWD] = true
				break
			}
		}
	}
	return dirs, nil
}

func (a *App) preferredCWD() string {
	if a.cur == nil {
		return ""
	}
	if pinned := a.defaults.Personas[a.cur.Name].CWD; pinned != "" {
		return pinned
	}
	dirs, err := a.priorDirectories()
	if err == nil && len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}
