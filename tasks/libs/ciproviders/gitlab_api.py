from __future__ import annotations

import json
import os
import platform
import re
import subprocess
import sys
from collections import UserList
from difflib import Differ

import gitlab
import yaml
from gitlab.v4.objects import Project, ProjectCommit, ProjectPipeline
from invoke.exceptions import Exit

from tasks.libs.common.color import Color, color_message
from tasks.libs.common.utils import retry_function

BASE_URL = "https://gitlab.ddbuild.io"


def get_gitlab_token():
    if "GITLAB_TOKEN" not in os.environ:
        print("GITLAB_TOKEN not found in env. Trying keychain...")
        if platform.system() == "Darwin":
            try:
                output = subprocess.check_output(
                    ['security', 'find-generic-password', '-a', os.environ["USER"], '-s', 'GITLAB_TOKEN', '-w']
                )
                if len(output) > 0:
                    return output.strip()
            except subprocess.CalledProcessError:
                print("GITLAB_TOKEN not found in keychain...")
                pass
        print(
            "Please create an 'api' access token at "
            "https://gitlab.ddbuild.io/-/profile/personal_access_tokens and "
            "add it as GITLAB_TOKEN in your keychain "
            "or export it from your .bashrc or equivalent."
        )
        raise Exit(code=1)
    return os.environ["GITLAB_TOKEN"]


def get_gitlab_bot_token():
    if "GITLAB_BOT_TOKEN" not in os.environ:
        print("GITLAB_BOT_TOKEN not found in env. Trying keychain...")
        if platform.system() == "Darwin":
            try:
                output = subprocess.check_output(
                    ['security', 'find-generic-password', '-a', os.environ["USER"], '-s', 'GITLAB_BOT_TOKEN', '-w']
                )
                if output:
                    return output.strip()
            except subprocess.CalledProcessError:
                print("GITLAB_BOT_TOKEN not found in keychain...")
                pass
        print(
            "Please make sure that the GITLAB_BOT_TOKEN is set or that " "the GITLAB_BOT_TOKEN keychain entry is set."
        )
        raise Exit(code=1)
    return os.environ["GITLAB_BOT_TOKEN"]


def get_gitlab_api(token=None) -> gitlab.Gitlab:
    """
    Returns the gitlab api object with the api token.
    The token is the one of get_gitlab_token() by default.
    """
    token = token or get_gitlab_token()

    return gitlab.Gitlab(BASE_URL, private_token=token, retry_transient_errors=True)


def get_gitlab_repo(repo='DataDog/datadog-agent', token=None) -> Project:
    api = get_gitlab_api(token)
    repo = api.projects.get(repo)

    return repo


def get_commit(project_name: str, git_sha: str) -> ProjectCommit:
    """
    Retrieves the commit for a given git sha a given project.
    """
    repo = get_gitlab_repo(project_name)
    return repo.commits.get(git_sha)


def get_pipeline(project_name: str, pipeline_id: str) -> ProjectPipeline:
    """
    Retrieves the pipeline for a given pipeline id in a given project.
    """
    repo = get_gitlab_repo(project_name)
    return repo.pipelines.get(pipeline_id)


@retry_function('refresh pipeline #{0.id}')
def refresh_pipeline(pipeline: ProjectPipeline):
    """
    Refresh a pipeline, retries if there is an error
    """
    pipeline.refresh()


class GitlabCIDiff:
    def __init__(self, before: dict, after: dict) -> None:
        """
        Used to display job diffs between two gitlab ci configurations
        """
        self.before = before
        self.after = after
        self.added_contents = {}
        self.modified_diffs = {}

        self.make_diff()

    def __bool__(self) -> bool:
        return bool(self.added or self.removed or self.modified or self.renamed)

    def make_diff(self):
        """
        Compute the diff between the two gitlab ci configurations
        """
        # Find added / removed jobs by names
        unmoved = self.before.keys() & self.after.keys()
        self.removed = self.before.keys() - unmoved
        self.added = self.after.keys() - unmoved
        self.renamed = set()

        # Find jobs that have been renamed
        for before_job in self.removed:
            for after_job in self.added:
                if self.before[before_job] == self.after[after_job]:
                    self.renamed.add((before_job, after_job))

        for before_job, after_job in self.renamed:
            self.removed.remove(before_job)
            self.added.remove(after_job)

        # Added jobs contents
        for job in self.added:
            self.added_contents[job] = yaml.safe_dump({job: self.after[job]})

        # Find modified jobs
        self.modified = set()
        for job in unmoved:
            if self.before[job] != self.after[job]:
                self.modified.add(job)

        # Modified jobs
        if self.modified:
            differcli = Differ()
            for job in self.modified:
                if self.before[job] == self.after[job]:
                    continue

                # Make diff
                before_content = yaml.safe_dump({job: self.before[job]}, default_flow_style=False, sort_keys=True)
                after_content = yaml.safe_dump({job: self.after[job]}, default_flow_style=False, sort_keys=True)
                before_content = before_content.splitlines()
                after_content = after_content.splitlines()

                diff = [line.rstrip('\n') for line in differcli.compare(before_content, after_content)]
                self.modified_diffs[job] = diff

    def display(self, cli: bool = True, max_detailed_jobs=6, job_url=None, only_summary=False) -> str:
        """
        Display in cli or markdown
        """

        def str_section(title, wrap=False) -> list[str]:
            if cli:
                return [f'--- {color_message(title, Color.BOLD)} ---']
            elif wrap:
                return ['<details>', f'<summary><h3>{title}</h3></summary>']
            else:
                return [f'### {title}']

        def str_end_section(wrap: bool) -> list[str]:
            if cli:
                return []
            elif wrap:
                return ['</details>']
            else:
                return []

        def str_job(title, color):
            if cli:
                return f'* {color_message(title, getattr(Color, color))}'
            else:
                return f'- **{title}**'

        def str_rename(job_before, job_after):
            if cli:
                return f'* {color_message(job_before, Color.GREY)} -> {color_message(job_after, Color.BLUE)}'
            else:
                return f'- {job_before} -> **{job_after}**'

        def str_add_job(name: str, content: str) -> list[str]:
            if cli:
                content = [color_message(line, Color.GREY) for line in content.splitlines()]

                return [str_job(name, 'GREEN'), '', *content, '']
            else:
                header = f'<summary><b>{name}</b></summary>'

                return ['<details>', header, '', '```yaml', *content.splitlines(), '```', '', '</details>']

        def str_modified_job(name: str, diff: list[str]) -> list[str]:
            if cli:
                res = [str_job(name, 'ORANGE')]
                for line in diff:
                    if line.startswith('+'):
                        res.append(color_message(line, Color.GREEN))
                    elif line.startswith('-'):
                        res.append(color_message(line, Color.RED))
                    else:
                        res.append(line)

                return res
            else:
                # Wrap diff in markdown code block and in details html tags
                return [
                    '<details>',
                    f'<summary><b>{name}</b></summary>',
                    '',
                    '```diff',
                    *diff,
                    '```',
                    '',
                    '</details>',
                ]

        def str_color(text: str, color: str) -> str:
            if cli:
                return color_message(text, getattr(Color, color))
            else:
                return text

        def str_summary() -> str:
            if cli:
                res = ''
                res += f'{len(self.removed)} {str_color("removed", "RED")}'
                res += f' | {len(self.modified)} {str_color("modified", "ORANGE")}'
                res += f' | {len(self.added)} {str_color("added", "GREEN")}'
                res += f' | {len(self.renamed)} {str_color("renamed", "BLUE")}'

                return res
            else:
                res = '| Removed | Modified | Added | Renamed |\n'
                res += '| ------- | -------- | ----- | ------- |\n'
                res += f'| {" | ".join(str(len(changes)) for changes in [self.removed, self.modified, self.added, self.renamed])} |'

                return res

        def str_note() -> list[str]:
            if not job_url or cli:
                return []

            return ['', f':information_source: *Diff available in the [job log]({job_url}).*']

        res = []

        if only_summary:
            if not cli:
                res.append(':warning: Diff too large to display on Github')
        else:
            if self.modified:
                wrap = len(self.modified) > max_detailed_jobs
                res.extend(str_section('Modified Jobs', wrap=wrap))
                for job, diff in sorted(self.modified_diffs.items()):
                    res.extend(str_modified_job(job, diff))
                res.extend(str_end_section(wrap=wrap))

            if self.added:
                if res:
                    res.append('')
                wrap = len(self.added) > max_detailed_jobs
                res.extend(str_section('Added Jobs', wrap=wrap))
                for job, content in sorted(self.added_contents.items()):
                    res.extend(str_add_job(job, content))
                res.extend(str_end_section(wrap=wrap))

            if self.removed:
                if res:
                    res.append('')
                res.extend(str_section('Removed Jobs'))
                for job in sorted(self.removed):
                    res.append(str_job(job, 'RED'))

            if self.renamed:
                if res:
                    res.append('')
                res.extend(str_section('Renamed Jobs'))
                for job_before, job_after in sorted(self.renamed):
                    res.append(str_rename(job_before, job_after))

        if self.added or self.renamed or self.modified or self.removed:
            if res:
                res.append('')
            res.extend(str_section('Changes Summary'))
            res.append(str_summary())
            res.extend(str_note())

        return '\n'.join(res)


class ReferenceTag(yaml.YAMLObject):
    """
    Custom yaml tag to handle references in gitlab-ci configuration
    """

    yaml_tag = '!reference'

    def __init__(self, references):
        self.references = references

    @classmethod
    def from_yaml(cls, loader, node):
        return UserList(loader.construct_sequence(node))

    @classmethod
    def to_yaml(cls, dumper, data):
        return dumper.represent_sequence(cls.yaml_tag, data.data, flow_style=True)


# Update loader/dumper to handle !reference tag
yaml.SafeLoader.add_constructor(ReferenceTag.yaml_tag, ReferenceTag.from_yaml)
yaml.SafeDumper.add_representer(UserList, ReferenceTag.to_yaml)

# HACK: The following line is a workaround to prevent yaml dumper from removing quote around comma separated numbers, otherwise Gitlab Lint API will remove the commas
yaml.SafeDumper.add_implicit_resolver(
    'tag:yaml.org,2002:int', re.compile(r'''^([0-9]+(,[0-9]*)*)$'''), list('0213456789')
)


def clean_gitlab_ci_configuration(yml):
    """
    - Remove `extends` tags
    - Flatten lists of lists
    """

    def flatten(yml):
        """
        Flatten lists (nesting due to !reference tags)
        """
        if isinstance(yml, list):
            res = []
            for v in yml:
                if isinstance(v, list):
                    res.extend(flatten(v))
                else:
                    res.append(v)

            return res
        elif isinstance(yml, dict):
            return {k: flatten(v) for k, v in yml.items()}
        else:
            return yml

    # Remove extends
    for content in yml.values():
        if 'extends' in content:
            del content['extends']

    # Flatten
    return flatten(yml)


def filter_gitlab_ci_configuration(yml: dict, job: str | None = None) -> dict:
    """
    Filters gitlab-ci configuration jobs

    - job: If provided, retrieve only this job
    """

    def filter_yaml(key, value):
        # Not a job
        if key.startswith('.') or 'script' not in value and 'trigger' not in value:
            return None

        if job is not None:
            return (key, value) if key == job else None

        return key, value

    if job is not None:
        assert job in yml, f"Job {job} not found in the configuration"

    return {node[0]: node[1] for node in (filter_yaml(k, v) for k, v in yml.items()) if node is not None}


def print_gitlab_ci_configuration(yml: dict, sort_jobs: bool):
    """
    Prints a gitlab ci as yaml.

    - sort_jobs: Sort jobs by name (the job keys are always sorted)
    """
    jobs = yml.items()
    if sort_jobs:
        jobs = sorted(jobs)

    for i, (job, content) in enumerate(jobs):
        if i > 0:
            print()
        yaml.safe_dump({job: content}, sys.stdout, default_flow_style=False, sort_keys=True, indent=2)


def get_full_gitlab_ci_configuration(
    ctx,
    input_file: str = '.gitlab-ci.yml',
    return_dict: bool = True,
    ignore_errors: bool = False,
    git_ref: str | None = None,
    input_config: dict | None = None,
) -> str | dict:
    """
    Returns the full gitlab-ci configuration by resolving all includes and applying postprocessing (extends / !reference)
    Uses the /lint endpoint from the gitlab api to apply postprocessing

    - input_config: If not None, will use this config instead of parsing existing yaml file at `input_file`
    """
    if not input_config:
        # Read includes
        concat_config = read_includes(ctx, input_file, return_config=True, git_ref=git_ref)
        assert concat_config
    else:
        concat_config = input_config

    agent = get_gitlab_repo()
    res = agent.ci_lint.create({"content": yaml.safe_dump(concat_config), "dry_run": True, "include_jobs": True})

    if not ignore_errors and not res.valid:
        errors = '; '.join(res.errors)
        raise RuntimeError(f"{color_message('Invalid configuration', Color.RED)}: {errors}")

    if return_dict:
        return yaml.safe_load(res.merged_yaml)
    else:
        return res.merged_yaml


def get_gitlab_ci_configuration(
    ctx,
    input_file: str = '.gitlab-ci.yml',
    ignore_errors: bool = False,
    job: str | None = None,
    clean: bool = True,
    git_ref: str | None = None,
) -> dict:
    """
    Creates, filters and processes the gitlab-ci configuration
    """

    # Make full configuration
    yml = get_full_gitlab_ci_configuration(ctx, input_file, ignore_errors=ignore_errors, git_ref=git_ref)

    # Filter
    yml = filter_gitlab_ci_configuration(yml, job)

    # Clean
    if clean:
        yml = clean_gitlab_ci_configuration(yml)

    return yml


def generate_gitlab_full_configuration(
    ctx, input_file, context=None, compare_to=None, return_dump=True, apply_postprocessing=False
):
    """
    Generate a full gitlab-ci configuration by resolving all includes

    - input_file: Initial gitlab yaml file (.gitlab-ci.yml)
    - context: Gitlab variables
    - compare_to: Override compare_to on change rules
    - return_dump: Whether to return the string dump or the dict object representing the configuration
    - apply_postprocessing: Whether or not to solve `extends` and `!reference` tags
    """
    if apply_postprocessing:
        full_configuration = get_full_gitlab_ci_configuration(ctx, input_file)
    else:
        full_configuration = read_includes(None, input_file, return_config=True)

    # Override some variables with a dedicated context
    if context:
        full_configuration["variables"].update(context)
    if compare_to:
        for value in full_configuration.values():
            if (
                isinstance(value, dict)
                and "changes" in value
                and isinstance(value["changes"], dict)
                and "compare_to" in value["changes"]
            ):
                value["changes"]["compare_to"] = compare_to
            elif isinstance(value, list):
                for v in value:
                    if (
                        isinstance(v, dict)
                        and "changes" in v
                        and isinstance(v["changes"], dict)
                        and "compare_to" in v["changes"]
                    ):
                        v["changes"]["compare_to"] = compare_to

    return yaml.safe_dump(full_configuration) if return_dump else full_configuration


def read_includes(ctx, yaml_files, includes=None, return_config=False, add_file_path=False, git_ref: str | None = None):
    """
    Recursive method to read all includes from yaml files and store them in a list
    - add_file_path: add the file path to each object of the parsed file
    """
    if includes is None:
        includes = []

    if isinstance(yaml_files, str):
        yaml_files = [yaml_files]

    for yaml_file in yaml_files:
        current_file = read_content(ctx, yaml_file, git_ref=git_ref)

        if add_file_path:
            for value in current_file.values():
                if isinstance(value, dict):
                    value['_file_path'] = yaml_file

        if 'include' not in current_file:
            includes.append(current_file)
        else:
            read_includes(ctx, current_file['include'], includes, add_file_path=add_file_path, git_ref=git_ref)
            del current_file['include']
            includes.append(current_file)

    # Merge all files
    if return_config:
        full_configuration = {}
        for yaml_file in includes:
            full_configuration.update(yaml_file)

        return full_configuration


def read_content(ctx, file_path, git_ref: str | None = None):
    """
    Read the content of a file, either from a local file or from an http endpoint
    """
    if file_path.startswith('http'):
        import requests

        response = requests.get(file_path)
        response.raise_for_status()
        content = response.text
    elif git_ref:
        content = ctx.run(f"git show '{git_ref}:{file_path}'", hide=True).stdout
    else:
        with open(file_path) as f:
            content = f.read()

    return yaml.safe_load(content)


def get_preset_contexts(required_tests):
    possible_tests = ["all", "main", "release", "mq", "conductor"]
    required_tests = required_tests.casefold().split(",")
    if set(required_tests) | set(possible_tests) != set(possible_tests):
        raise Exit(f"Invalid test required: {required_tests} must contain only values from {possible_tests}", 1)
    main_contexts = [
        ("BUCKET_BRANCH", ["nightly"]),  # ["dev", "nightly", "beta", "stable", "oldnightly"]
        ("CI_COMMIT_BRANCH", ["main"]),  # ["main", "mq-working-branch-main", "7.42.x", "any/name"]
        ("CI_COMMIT_TAG", [""]),  # ["", "1.2.3-rc.4", "6.6.6"]
        ("CI_PIPELINE_SOURCE", ["pipeline"]),  # ["trigger", "pipeline", "schedule"]
        ("DEPLOY_AGENT", ["true"]),
        ("RUN_ALL_BUILDS", ["true"]),
        ("RUN_E2E_TESTS", ["auto"]),
        ("RUN_KMT_TESTS", ["on"]),
        ("RUN_UNIT_TESTS", ["on"]),
        ("TESTING_CLEANUP", ["true"]),
    ]
    release_contexts = [
        ("BUCKET_BRANCH", ["stable"]),
        ("CI_COMMIT_BRANCH", ["7.42.x"]),
        ("CI_COMMIT_TAG", ["3.2.1", "1.2.3-rc.4"]),
        ("CI_PIPELINE_SOURCE", ["schedule"]),
        ("DEPLOY_AGENT", ["true"]),
        ("RUN_ALL_BUILDS", ["true"]),
        ("RUN_E2E_TESTS", ["auto"]),
        ("RUN_KMT_TESTS", ["on"]),
        ("RUN_UNIT_TESTS", ["on"]),
        ("TESTING_CLEANUP", ["true"]),
    ]
    mq_contexts = [
        ("BUCKET_BRANCH", ["dev"]),
        ("CI_COMMIT_BRANCH", ["mq-working-branch-main"]),
        ("CI_PIPELINE_SOURCE", ["pipeline"]),
        ("DEPLOY_AGENT", ["false"]),
        ("RUN_ALL_BUILDS", ["false"]),
        ("RUN_E2E_TESTS", ["auto"]),
        ("RUN_KMT_TESTS", ["off"]),
        ("RUN_UNIT_TESTS", ["off"]),
        ("TESTING_CLEANUP", ["false"]),
    ]
    conductor_contexts = [
        ("BUCKET_BRANCH", ["nightly"]),  # ["dev", "nightly", "beta", "stable", "oldnightly"]
        ("CI_COMMIT_BRANCH", ["main"]),  # ["main", "mq-working-branch-main", "7.42.x", "any/name"]
        ("CI_COMMIT_TAG", [""]),  # ["", "1.2.3-rc.4", "6.6.6"]
        ("CI_PIPELINE_SOURCE", ["pipeline"]),  # ["trigger", "pipeline", "schedule"]
        ("DDR_WORKFLOW_ID", ["true"]),
    ]
    all_contexts = []
    for test in required_tests:
        if test in ["all", "main"]:
            generate_contexts(main_contexts, [], all_contexts)
        if test in ["all", "release"]:
            generate_contexts(release_contexts, [], all_contexts)
        if test in ["all", "mq"]:
            generate_contexts(mq_contexts, [], all_contexts)
        if test in ["all", "conductor"]:
            generate_contexts(conductor_contexts, [], all_contexts)
    return all_contexts


def generate_contexts(contexts, context, all_contexts):
    """
    Recursive method to generate all possible contexts from a list of tuples
    """
    if len(contexts) == 0:
        all_contexts.append(context[:])
        return
    for value in contexts[0][1]:
        context.append((contexts[0][0], value))
        generate_contexts(contexts[1:], context, all_contexts)
        context.pop()


def load_context(context):
    """
    Load a context either from a yaml file or from a json string
    """
    if os.path.exists(context):
        with open(context) as f:
            y = yaml.safe_load(f)
        if "variables" not in y:
            raise Exit(
                f"Invalid context file: {context}, missing 'variables' key. Input file must be similar to tasks/unit-tests/testdata/gitlab_main_context_template.yml",
                1,
            )
        return [list(y["variables"].items())]
    else:
        try:
            j = json.loads(context)
            return [list(j.items())]
        except json.JSONDecodeError as e:
            raise Exit(f"Invalid context: {context}, must be a valid json, or a path to a yaml file", 1) from e
