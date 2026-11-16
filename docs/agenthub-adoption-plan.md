# Plan: find, adopt and configure TECHO5 Dots from AgentHub

Status: plan only, nothing built. Branch `agenthub-voice` on synssins/techo5, together with AgentHub.

## Goal

In AgentHub, **Settings > Voice > Devices > Find Dots**:

1. AgentHub lists the TECHO5 Dots on the network, found by mDNS or by an IP address typed in.
2. You pick one.
3. If it isn't set up yet, AgentHub adopts it.
4. AgentHub then sets it up as one of its voice devices: its own device key and channel, the AgentHub address and key on the Dot, **Answered by: AgentHub**, and listening on.

Home Assistant is not needed for any of it.

## What TECHO5 already has (no device change needed for most of this)

- **mDNS:** each Dot advertises `_esphomelib._tcp` with TXT records `friendly_name`, `version`, `mac`, `platform`, `board` and `network`.
  - Its key state is in the TXT too. `api_encryption=…` means the Dot already has a key: some Home Assistant owns it.
  - `api_encryption_supported` with `api_provisioning=zero-psk` means it is waiting to be adopted (`internal/feature/api/mdns_panel.go`, `mdns.go`).
- **Adoption window** (`internal/feature/api/adopt.go`):
  - Opened from the Dot's setup page, after a press on the Dot itself. That press is the consent: somebody is standing at it.
  - For 15 minutes the Dot serves the ESPHome API with the all-zeros key. The first client that adds it sets its own key.
  - Whoever owned the Dot before loses it.
- **Every setting AgentHub needs is already an ESPHome entity** (feature/agenthub):

| Entity | Purpose |
|---|---|
| `agenthub_url` | AgentHub address |
| `agenthub_key` | device key; masked, write-only |
| `brain` | Answered by: Home Assistant / Chat model / AgentHub |
| `agenthub_listen` | listen without a wake word |
| `replies_play_on` | where replies play |
| chat-model fields | for the direct-LLM option |

  These are read-only through TECHO5's separate remote API on purpose. AgentHub uses the ESPHome API instead, which is where Home Assistant sets them too.

## How AgentHub does it

1. **Find.** Browse `_esphomelib._tcp` with python-zeroconf and keep the records whose TXT says TECHO5.
   - Use `platform`/`board`; add a `project=techo5-dot` TXT entry if they don't make TECHO5 unambiguous (a one-line TECHO5 change).
   - Typing an IP or hostname does one direct ESPHome API `hello` and `device_info` instead.
2. **Show state** from the TXT:

| TXT says | AgentHub shows |
|---|---|
| `zero-psk` | Ready to adopt |
| `api_encryption` | Owned by another controller (usually Home Assistant) |
| known to AgentHub already | Adopted by this AgentHub |

3. **Adopt** a Dot that is ready:
   - AgentHub connects with `aioesphomeapi` over zero-key Noise, exactly as Home Assistant does, and sets a fresh random key.
   - It keeps the key in `data/voice_dots.json` (mode 600, never shown).
   - A Dot owned by Home Assistant can't be adopted without taking it from Home Assistant. Two choices:
     - **Take it over:** the person opens the adoption window on the Dot first, as the Dot already requires.
     - **Share it:** paste the Dot's existing key (from Home Assistant's ESPHome integration). AgentHub then connects alongside Home Assistant with the same key, and Home Assistant keeps working.
4. **Configure** over the ESPHome API:
   - Create the AgentHub voice device (`voice_devices.create`) with a channel named after the Dot's room.
   - Write the AgentHub address, the device key and `brain = AgentHub` to the Dot. Turn listening on only if chosen.
   - Read the settings back to confirm.
5. **Afterwards** the Devices list shows each Dot's status from its `agenthub_status` sensor (listening / key refused / unreachable). AgentHub keeps an ESPHome connection only while you're on that page; it doesn't hold one open all the time.

## What would need building

| Where | What |
|---|---|
| AgentHub | `voice_dots.py`: mDNS browse, ESPHome client (`aioesphomeapi`, new dependency), adopt, read and write entities. Admin routes `/api/admin/voice-dots/scan`, `/adopt`, `/configure`. Settings > Voice > Devices: a "Find Dots" list with Adopt and Configure buttons. Tests against a stub ESPHome device. |
| TECHO5 fork | Possibly a `project=techo5-dot` TXT entry. Nothing else: the adoption window and entities exist. |

## Risks and open questions

- **One key per Dot.** Home Assistant and AgentHub must share it, or one owns the Dot alone. Sharing needs the key pasted once.
- **Adoption needs someone at the Dot,** pressing its button to open the window. That's deliberate and should stay: a remote take-over of a microphone is the thing to prevent.
- **The device key travels to the Dot over the ESPHome API.** That link is encrypted once the Dot has a key; during adoption it uses zero-key Noise for those few seconds, as Home Assistant's adoption does.
- **`aioesphomeapi` versions** track Home Assistant's. Pin a version AgentHub tests against.
