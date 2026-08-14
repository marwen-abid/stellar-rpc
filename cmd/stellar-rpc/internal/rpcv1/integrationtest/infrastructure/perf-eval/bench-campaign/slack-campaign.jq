# Renders the campaign ok or fail branch as one Slack attachment.
# Important blocks precede secondary fields and context that may fold behind “Show more”.
  def button($t; $u): {type: "button", text: {type: "plain_text", text: $t, emoji: true}, url: $u};
  def pbutton($t; $u): button($t; $u) + {style: "primary"};
  def field($l; $v): {type: "mrkdwn", text: "*\($l)*\n\($v)"};

  (if $sha != "" then "\($ref) @ <\($repo)/commit/\($sha)|\($sha[0:8])>" else $ref end) as $reftxt
  | (if $elapsed != "" and $budget != "" then " in *\($elapsed)* of a \($budget) budget"
     elif $elapsed != "" then " in *\($elapsed)*" else "" end) as $took

  | (if $state == "ok" then
      {
        color: "#2eb67d",
        header: "✅ Bench campaign passed — \($name | .[0:100])",
        lead: "\(if $runs == "1" then "The run finished green" else "All *\($runs) runs* green" end)\($took).",
        fields: ([
          ["Phase", $phase],
          ["Machine", ($machine + (if $workers != "" then " · \($workers) workers" else "" end))],
          ["Benchmarked ref", $reftxt],
          ["Run id", (if $bench != "" then "`\($bench)`" else "" end)]
        ] | map(select(.[1] != "") | field(.[0]; .[1]))),
        fields2: ([
          ["Ingest", ($ingest + (if $hot_cap != "" and $hot_cap != "0" then " · capped at \($hot_cap) ledgers" else "" end))],
          ["Query", $query]
        ] | map(select(.[1] != "") | field(.[0]; .[1]))),
        quote: "",
        buttons: ([
          (if $summary != "" then pbutton("View results"; $summary) else empty end),
          (if $run_url != "" then button("GitHub run"; $run_url) else empty end),
          (if $bundle_url != "" then button("Bundle on S3"; $bundle_url) else empty end)
        ]),
        context: ([
          (if $summary != "" then "Ingested into the results site" else "Not on the results site — the ingest did not complete" end),
          (if $harness != "" then "harness \($harness | .[0:8])" else empty end)
        ] | join(" · "))
      }
    else
      {
        color: "#e01e5a",
        header: "❌ Bench campaign failed — \($name | .[0:100])",
        lead: "*Reason:* \($reason)\(if $elapsed != "" and $budget != "" then ", after \($elapsed) of a \($budget) budget" else "" end).",
        fields: ([
          ["Phase / Machine", ([$phase, $machine] | map(select(. != "")) | join(" · "))],
          ["Benchmarked ref", $reftxt],
          ["Box", (if $box != "" and $rescued == "true" then "`\($box)` — left up for rescue" else "" end)],
          ["Run-id tag", (if $runidtag != "" and $rescued == "true" then "`\($runidtag)`" else "" end)]
        ] | map(select(.[1] != "") | field(.[0]; .[1]))),
        fields2: [],
        quote: (if $excerpt != "" then ($excerpt | split("\n") | map(select(. != "") | "> " + .) | join("\n")) else "" end),
        buttons: ([
          (if $run_url != "" then pbutton("GitHub run"; $run_url) else empty end),
          (if $boxlog_url != "" then button("Box log"; $boxlog_url) else empty end),
          (if $verdict_url != "" then button("Verdict on S3"; $verdict_url) else empty end)
        ]),
        context: (
          if $box != "" and $rescued == "true" then
            "⚠️ The box stays up for debugging — terminate it by the run-id tag when done, or the reaper kills it at the deadline\(if $deadline_hm != "" then " (\($deadline_hm) UTC)" else "" end)."
          elif $box != "" then "Box \($box) was terminated."
          else "No box was launched." end)
      }
    end) as $m

  | {
      attachments: [{
        color: $m.color,
        fallback: $text,
        blocks: (
          [{type: "header", text: {type: "plain_text", text: $m.header, emoji: true}}]
          + [{type: "section", text: {type: "mrkdwn", text: $m.lead}}]
          + (if $m.quote != "" then [{type: "section", text: {type: "mrkdwn", text: $m.quote}}] else [] end)
          + (if ($m.fields | length) > 0 then [{type: "section", fields: $m.fields}] else [] end)
          + (if ($m.buttons | length) > 0 then [{type: "actions", elements: $m.buttons}] else [] end)
          + (if $results != null then
              [{type: "divider"},
               {type: "section", text: {type: "mrkdwn",
                 text: (([$results.header] + $results.lines + [$results.footer]) | join("\n"))}}]
             else [] end)
          + (if ($m.fields2 | length) > 0 then [{type: "section", fields: $m.fields2}] else [] end)
          + (if $m.context != "" then [{type: "context", elements: [{type: "mrkdwn", text: $m.context}]}] else [] end)
        )
      }]
    }
