# Fictional GroupMe exports

Every name, ID, date, message, URL and media byte in these fixtures was authored
for msgvault. These are fictional structural fixtures, not redacted real exports.
Tests create fictional media bytes in temporary gallery files.

Structural references studied for this adapter:

| Source | Full commit | License / use |
| --- | --- | --- |
| [groupme-viewer](https://github.com/jwlarocque/groupme-viewer/tree/daff07c45c9adcccea5ba2865779c27ed0298ecf) | `daff07c45c9adcccea5ba2865779c27ed0298ecf` | MIT; `types.go`, reader, public structural sample and frontend gallery converters establish the conversation object, message array, common fields, and exact image/video naming |
| [GroupMe-Export-Parser](https://github.com/theTrueEnder/GroupMe-Export-Parser/tree/2ff7bc54bb6fe4365094ff64d2ca6e4e7e7e7ffb) | `2ff7bc54bb6fe4365094ff64d2ca6e4e7e7e7ffb` | MIT; common message and attachment field facts |
| [GroupMe-Export-Tools](https://github.com/michaelskc/GroupMe-Export-Tools/tree/e7827a8d23205eba8cf99b583c651732680795db) | `e7827a8d23205eba8cf99b583c651732680795db` | MIT; common message field facts |
| [ChatStat](https://github.com/Lazersmoke/ChatStat/tree/2d99d5f322574441a5f8356704efc42a7e4f5b1b) | `2d99d5f322574441a5f8356704efc42a7e4f5b1b` | MIT; common field corroboration |
| [Message-Nexus](https://github.com/Zackaryia/Message-Nexus/blob/96ea5811effbe84d9c32a62eb00660cbbc830dd1/parser/services/group_me_json_export.py) | `96ea5811effbe84d9c32a62eb00660cbbc830dd1` | No license found; field/layout facts only, including `conversation_id` and optional `chat.other_user.name` for DMs, and `sender_type` service/system classification |
| [Parser wiki](https://github.com/theTrueEnder/GroupMe-Export-Parser/wiki) | `7d97d137f8049c5d8d6198db399e9f887045e12d` | No separate wiki license established; facts only |
| [battleplan](https://github.com/jfreema/battleplan/blob/437f198c4ac8c76ff426ea7d8820700930d02083/lib/features/imports/domain/groupme_export.dart) | `437f198c4ac8c76ff426ea7d8820700930d02083` | No license found; facts only |

No implementation or fixture data was copied from these sources. The public
structural sample's provenance does not establish whether it was generated,
redacted or handwritten. Its message had no media. Gallery mappings come from
the viewer's image/video URL converters, not from a verified official media sample.
No current official versioned export schema or complete DM metadata sample was
found. Uploaded-file gallery matching is intentionally unsupported.

[Microsoft's export guide](https://support.microsoft.com/en-us/groupme/how-do-i-export-my-groupme-data)
describes selected conversations and DMs in JSON and one active export.
[Retention](https://support.microsoft.com/en-us/groupme/how-long-are-files-and-data-available-in-groupme)
limits media availability to five years and has ambiguous message wording about
April 2017 and deletion. These fixtures make no lifetime-history claim.
