// The signed-in user's role. The server enforces it; the UI uses it only to
// avoid showing controls that would be refused.
import { createContext, useContext } from "react";
import type { Role } from "./api";

const rank: Record<string, number> = { viewer: 1, operator: 2, admin: 3 };

export const RoleContext = createContext<Role>("viewer");
export const useRole = () => useContext(RoleContext);
export const atLeast = (have: Role, need: Role) => (rank[have] ?? 0) >= rank[need];
