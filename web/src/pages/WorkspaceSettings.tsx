import { VStack } from "@astryxdesign/core/Stack";

import { Page } from "../components/Page";
import { TemplatesCard } from "./workspace/panels";
import WorkspacesCard from "./settings/WorkspacesCard";

export default function WorkspaceSettings() {
  return (
    <Page
      eyebrow="Develop"
      title="Workspace settings"
      description="Where workspaces run, what they may use, and the templates people pick from."
    >
      <VStack gap={4}>
        <TemplatesCard />
        <VStack maxWidth={760}>
          <WorkspacesCard />
        </VStack>
      </VStack>
    </Page>
  );
}
