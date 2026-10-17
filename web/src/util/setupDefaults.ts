/**
 * What the first-run wizard pre-fills and where it lands once setup completes.
 */

/**
 * Where "Finish setup" lands: Devices, where the next step (adding a rig) is.
 * Settings would greet a first-time user with every knob at once.
 */
export const SETUP_LANDING = "/devices";

/**
 * The light-frames and staging folders the wizard pre-fills on a Windows server,
 * or null elsewhere (a container's folders are whatever was mounted, so there is
 * no default worth guessing). The user can change both.
 *
 * They sit under the shared Public Pictures folder rather than the user's own
 * Pictures: Commander runs as the virtual account NT SERVICE\ASIAIRCommander,
 * which a user profile's ACL (SYSTEM, Administrators, the owner) does not let
 * write, while Windows lets local services write under C:\Users\Public. It is
 * also where a person looks for pictures, unlike %ProgramData%, which is hidden
 * and which the installer locks to administrators. Both are on one drive, as the
 * single-volume rule for staging and the trees requires. Were the ACL ever
 * different, the wizard's folder check names the folder and the account to
 * grant before setup completes.
 */
export function defaultRoots(os: string | undefined): { light: string; staging: string } | null {
  if (os !== "windows") return null;
  const base = "C:\\Users\\Public\\Pictures\\ASIAIR";
  return { light: `${base}\\Lights`, staging: `${base}\\_staging` };
}
