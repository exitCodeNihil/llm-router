/**
 * Application frame.
 *
 * Responsive contract (declared at the frame root, per Astryx layout docs):
 *   > 1024px  side nav 260 (collapsible, resizable) | content
 *   <= 1024px side nav collapses to the icon rail; content takes the width
 *   <= 768px  AppShell swaps the rail for the MobileNav drawer automatically;
 *             the top nav keeps identity + account actions only
 */
import type { ComponentProps, ElementType } from "react";
import { Link as RouterLink, useLocation, useNavigate } from "react-router-dom";
import { AppShell } from "@astryxdesign/core/AppShell";
import {
  SideNav,
  SideNavItem,
  SideNavSection,
} from "@astryxdesign/core/SideNav";
import { TopNav, TopNavHeading } from "@astryxdesign/core/TopNav";
import { NavIcon } from "@astryxdesign/core/NavIcon";
import { HStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { IconButton } from "@astryxdesign/core/IconButton";
import { DropdownMenu } from "@astryxdesign/core/DropdownMenu";
import { Icon } from "@astryxdesign/core/Icon";
import { Badge } from "@astryxdesign/core/Badge";
import { useMediaQuery } from "@astryxdesign/core/hooks";

import type { Me } from "../lib/types";
import { token } from "../lib/api";
import { useMode } from "../mode";
import {
  IconChat,
  IconWorkspace,
  IconAnalytics,
  IconDashboard,
  IconDeployment,
  IconKey,
  IconMoon,
  IconEdge,
  IconObservability,
  IconPricing,
  IconProvider,
  IconRequests,
  IconSettings,
  IconSignal,
  IconSun,
  IconTeams,
  IconUser,
  IconUsers,
} from "./icons";

/**
 * Adapter so Astryx nav items render real react-router anchors: keeps
 * cmd/ctrl-click, middle-click and "copy link address" working, which a
 * plain onClick handler would silently break.
 */
const NavLink: ElementType = ({ href, ...rest }: { href?: string }) => (
  <RouterLink to={href ?? "/"} {...rest} />
);

type NavEntry = {
  to: string;
  label: string;
  icon: ComponentProps<typeof SideNavItem>["icon"];
  adminOnly?: boolean;
  /** Shown only when the account may use workspaces. */
  workspaces?: boolean;
  /** Shown only to non-admins (admins have the same page under Routing). */
  memberOnly?: boolean;
};

const SECTIONS: { title: string; adminOnly?: boolean; items: NavEntry[] }[] = [
  {
    title: "Overview",
    items: [
      { to: "/", label: "Dashboard", icon: IconDashboard },
      { to: "/analytics", label: "Analytics", icon: IconAnalytics },
      { to: "/requests", label: "Requests", icon: IconRequests },
    ],
  },
  {
    title: "Develop",
    items: [
      { to: "/chat", label: "Playground", icon: IconChat },
      {
        to: "/models",
        label: "Models",
        icon: IconDeployment,
        memberOnly: true,
      },
      {
        to: "/workspaces",
        label: "Workspaces",
        icon: IconWorkspace,
        workspaces: true,
      },
      {
        to: "/workspaces/settings",
        label: "Workspace settings",
        icon: IconSettings,
        adminOnly: true,
      },
    ],
  },
  {
    title: "Access",
    items: [
      { to: "/keys", label: "API Keys", icon: IconKey },
      { to: "/teams", label: "Teams", icon: IconTeams },
    ],
  },
  {
    title: "Routing",
    adminOnly: true,
    items: [
      { to: "/providers", label: "Providers", icon: IconProvider },
      { to: "/models", label: "Models", icon: IconDeployment },
      { to: "/pricing", label: "Pricing", icon: IconPricing },
    ],
  },
  {
    title: "Operations",
    adminOnly: true,
    items: [
      { to: "/users", label: "Users", icon: IconUsers },
      { to: "/edge-nodes", label: "Edge nodes", icon: IconEdge },
      { to: "/observability", label: "Observability", icon: IconObservability },
      { to: "/settings", label: "Settings", icon: IconSettings },
    ],
  },
];

function AccountMenu({ me, isCompact }: { me: Me; isCompact: boolean }) {
  const navigate = useNavigate();
  const label = me.email || "admin";
  return (
    <DropdownMenu
      // On narrow viewports the email would push the nav row past the
      // viewport, so collapse the trigger to an icon-only button.
      button={
        isCompact
          ? {
              label,
              variant: "ghost",
              size: "sm",
              isIconOnly: true,
              icon: <Icon icon={IconUser} size="sm" />,
            }
          : { label, variant: "ghost", size: "sm" }
      }
      hasChevron={!isCompact}
      menuWidth={220}
      items={[
        {
          type: "section",
          title: label,
          items: [{ label: "Account", onClick: () => navigate("/account") }],
        },
        { type: "divider" },
        {
          label: "Sign out",
          onClick: () => {
            token.clear();
            // The server clears the HttpOnly session cookie; a plain reload
            // would sign a cookie session straight back in.
            window.location.href = "/auth/logout";
          },
        },
      ]}
    />
  );
}

export default function Shell({
  me,
  children,
}: {
  me: Me;
  children: React.ReactNode;
}) {
  const { pathname } = useLocation();
  const { mode, toggle } = useMode();
  const isCompact = useMediaQuery("(max-width: 600px)");

  // Longest matching prefix wins, so /workspaces/settings lights up its own
  // entry rather than both it and /workspaces.
  const active = SECTIONS.flatMap((s) => s.items)
    .map((i) => i.to)
    .filter((to) =>
      to === "/"
        ? pathname === "/"
        : pathname === to || pathname.startsWith(to + "/"),
    )
    .sort((a, b) => b.length - a.length)[0];
  const isActive = (to: string) => to === active;

  return (
    <AppShell
      height="fill"
      contentPadding={0}
      variant="section"
      topNav={
        <TopNav
          label="Main navigation"
          heading={
            <TopNavHeading
              heading="llm-router"
              logo={<NavIcon icon={<IconSignal width={16} height={16} />} />}
            />
          }
          endContent={
            <HStack gap={2} vAlign="center">
              {me.is_admin && !isCompact && (
                <Badge variant="neutral" label="admin" />
              )}
              <IconButton
                label={
                  mode === "dark"
                    ? "Switch to light mode"
                    : "Switch to dark mode"
                }
                variant="ghost"
                size="sm"
                icon={
                  <Icon icon={mode === "dark" ? IconSun : IconMoon} size="sm" />
                }
                onClick={toggle}
              />
              <AccountMenu me={me} isCompact={isCompact} />
            </HStack>
          }
        />
      }
      sideNav={
        <SideNav
          collapsible
          resizable={{
            defaultWidth: 260,
            minWidth: 200,
            maxWidth: 360,
            autoSaveId: "llmr-nav",
          }}
          footer={
            <Text type="supporting" color="secondary">
              <span className="eyebrow">llm-router</span>
            </Text>
          }
        >
          {SECTIONS.filter((s) => !s.adminOnly || me.is_admin).map(
            (section) => (
              <SideNavSection key={section.title} title={section.title}>
                {section.items
                  .filter(
                    (item) =>
                      (!item.adminOnly || me.is_admin) &&
                      (!item.memberOnly || !me.is_admin) &&
                      (!item.workspaces || me.workspaces),
                  )
                  .map((item) => (
                    <SideNavItem
                      key={item.to}
                      as={NavLink}
                      href={item.to}
                      label={item.label}
                      icon={item.icon}
                      isSelected={isActive(item.to)}
                    />
                  ))}
              </SideNavSection>
            ),
          )}
        </SideNav>
      }
    >
      {children}
    </AppShell>
  );
}
