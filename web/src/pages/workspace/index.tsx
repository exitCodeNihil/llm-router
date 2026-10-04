import { useNavigate, useParams } from "react-router-dom";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Code } from "@astryxdesign/core/Code";
import { Divider } from "@astryxdesign/core/Divider";
import { HStack, StackItem, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";

import { useMe, useWorkspaces } from "../../lib/hooks";
import { Loading } from "../../components/Page";

const fill: React.CSSProperties = { height: "100%", minHeight: 0 };

export default function WorkspaceIDE() {
  const me = useMe();
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const workspaces = useWorkspaces();
  const ws = workspaces.data?.find((w) => w.id === id);

  // Trailing slash matters: without it the IDE's relative asset URLs resolve
  // one level too high.
  // On its own origin when the gateway has one (LLMR_IDE_ORIGIN), so nothing
  // rendered inside the workspace can call this console's API as you.
  const ideURL = `${me.data?.ide_origin ?? ""}/api/workspaces/${id}/ide/`;

  if (workspaces.isLoading) return <Loading />;
  if (!ws) {
    return (
      <VStack gap={3} padding={5} hAlign="center">
        <Text type="body">That workspace does not exist, or is not yours.</Text>
        <Button label="Back to workspaces" variant="secondary" onClick={() => navigate("/workspaces")} />
      </VStack>
    );
  }
  if (ws.status !== "running") {
    return (
      <VStack gap={3} padding={5} hAlign="center">
        <Text type="body">
          <strong>{ws.name}</strong> is {ws.status}. Start it to open the editor.
        </Text>
        <Button label="Back to workspaces" variant="secondary" onClick={() => navigate("/workspaces")} />
      </VStack>
    );
  }

  return (
    <VStack gap={0} style={fill}>
      <HStack gap={3} padding={2} vAlign="center" hAlign="between">
        <HStack gap={2} vAlign="center">
          <Button label="Workspaces" variant="ghost" size="sm" onClick={() => navigate("/workspaces")} />
          <Text type="body" weight="medium">
            {ws.name}
          </Text>
          <Code>{ws.image}</Code>
        </HStack>
        <HStack gap={2} vAlign="center">
          <Text type="supporting" color="secondary">
            pi is on the terminal: <Code>pi</Code>
          </Text>
          {/* Browsers swallow some IDE shortcuts inside an iframe; a real tab
              gets all of them. */}
          <Button
            label="Open in a tab"
            variant="ghost"
            size="sm"
            onClick={() => window.open(ideURL, "_blank", "noopener")}
          />
          <Badge variant="success" label="running" />
        </HStack>
      </HStack>
      <Divider />
      <StackItem size="fill" style={{ minHeight: 0 }}>
        <iframe
          src={ideURL}
          title={`${ws.name} editor`}
          style={{ width: "100%", height: "100%", border: 0, display: "block" }}
          allow="clipboard-read; clipboard-write"
        />
      </StackItem>
    </VStack>
  );
}
