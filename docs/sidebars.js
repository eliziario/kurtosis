/**
 * Creating a sidebar enables you to:
 - create an ordered group of docs
 - render a sidebar for each doc of that group
 - provide next/previous navigation

 The sidebars can be generated from the filesystem, or explicitly defined here.

 Create as many sidebars as you want.
 */

// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
    main: [
        'home',
        'quickstart',
        {
            type: 'category',
            label: 'Guides',
            collapsed: true,
            items: [
                {type: 'autogenerated', dirName: 'guides'}
            ]
        },
        {
            type: 'category',
            label: 'Explanations',
            collapsed: true,
            items: [
                {type: 'autogenerated', dirName: 'explanations'}
            ]
        },
        {
            type: 'category',
            label: 'Reference',
            collapsed: true,
            items: [
                {type: 'autogenerated', dirName: 'reference'}
            ]
        },
        {
            type: 'link',
            label: 'Examples',
            href: 'https://github.com/kurtosis-tech/examples',
        },
        {
            type: 'link',
            label: 'Kurtosis-Managed Packages',
            href: 'https://github.com/kurtosis-tech?q=package+in%3Aname&type=all&language=&sort=',
        },
        'changelog',
    ],
};

module.exports = sidebars;
