# Reaper notification. Inputs: $reaped ([{id, runId, overdueMin}]), $untagged, $run_url, $repo.
def button($t; $u): {type: "button", text: {type: "plain_text", text: $t, emoji: true}, url: $u};
($reaped | length) as $n
| ($untagged | split(" ") | map(select(. != ""))) as $loose
| {
    attachments: [{
      color: "#ecb22e",
      fallback: "Reaper: \($n) terminated; \($loose | length) without usable deadlines. \($run_url)",
      blocks: (
        [{type: "header", text: {type: "plain_text",
          text: "⚠️ Reaper terminated \($n) past-deadline box\(if $n == 1 then "" else "es" end)", emoji: true}}]
        + [{type: "section", text: {type: "mrkdwn", text: (
            ( $reaped | map(
                (if .overdueMin >= 90 then "\((.overdueMin / 6 | round) / 10) h" else "\(.overdueMin) min" end) as $late
                | (if (.runId // "") != "" then "campaign run <\($repo)/actions/runs/\(.runId)|\(.runId)>" else "campaign run unknown" end) as $src
                | "• `\(.id)` — \($src), deadline passed *\($late)* ago"
              ) )
            + ( $loose | map("• `\(.)` has no deadline tag — left alone (hand-launched?)") )
            | join("\n")
            | if length == 0 then "No expired boxes."
              elif length > 3000 then .[0:2950] + "\n[truncated; see reaper run]" else . end
          )}}]
        + (if $run_url != "" then
            [{type: "actions", elements: [button("Reaper run"; $run_url)]}] else [] end)
        + [{type: "context", elements: [{type: "mrkdwn",
            text: "A reap means a campaign box outlived every in-band ceiling — worth a human look."}]}]
      )
    }]
  }
